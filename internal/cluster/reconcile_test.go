package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/local"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// reconcileFake is a storage whose Init behaves like 115 Open's with a dead
// pair: it reports a provider 401 through the real credential hook and fails.
// Any other pair initializes. Init of the dead pair can be held at a gate so a
// test controls when the storage becomes registered in op's storage map.
type reconcileFake struct {
	model.Storage
	addition struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
}

var (
	reconcileGateMu sync.Mutex
	reconcileGate   chan struct{}
)

func setReconcileGate(g chan struct{}) {
	reconcileGateMu.Lock()
	reconcileGate = g
	reconcileGateMu.Unlock()
}

func init() {
	op.RegisterDriver(func() driver.Driver { return &reconcileFake{} })
}

func (d *reconcileFake) Config() driver.Config {
	return driver.Config{Name: "ReconcileFake", NoCache: true, NoUpload: true}
}
func (d *reconcileFake) GetAddition() driver.Additional { return &d.addition }
func (d *reconcileFake) Drop(ctx context.Context) error { return nil }
func (d *reconcileFake) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	return nil, nil
}
func (d *reconcileFake) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return &model.Link{Header: http.Header{}}, nil
}
func (d *reconcileFake) Init(ctx context.Context) error {
	if d.addition.AccessToken == "expired" {
		reconcileGateMu.Lock()
		gate := reconcileGate
		reconcileGateMu.Unlock()
		if gate != nil {
			<-gate
		}
		st := d.GetStorage()
		op.NotifyStorageTokenInvalidWithSnapshot(d, st.Addition, st.Modified)
		// The credential hook runs in its own goroutine; let it finish while the
		// storage is still unregistered, which is the production ordering.
		time.Sleep(300 * time.Millisecond)
		return &sdk.Error{Code: sdk.CodeRefreshTokenError, Message: "dead pair"}
	}
	return nil
}

var reconcileSeq atomic.Int64

func reconcileDB(t *testing.T) {
	t.Helper()
	dbName := fmt.Sprintf("file:cluster-reconcile-%d?mode=memory&cache=shared", time.Now().UnixNano())
	database, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)
}

func signedRecord(peer *identity, mount, token string, version uint64) *credRecord {
	payload := map[string]json.RawMessage{"access_token": json.RawMessage(fmt.Sprintf("%q", token))}
	r := &credRecord{
		GroupID:      "g1",
		OriginDriver: "ReconcileFake",
		Fields:       []string{"access_token"},
		Payload:      payload,
		CredHash:     credHash(payload),
		Version:      version,
		Origin:       peer.NodeID,
		OriginPub:    peer.Pub,
		OriginMount:  mount,
		UpdatedAt:    now(),
	}
	r.Sig = peer.sign(r.signingBytes())
	r.MountSig = peer.sign(r.mountSigningBytes())
	return r
}

func rowStatus(mount string) (model.Storage, bool) {
	s, err := db.GetStorageByMountPath(mount)
	if err != nil {
		return model.Storage{}, false
	}
	return *s, true
}

// T1: the 2026-10-29 bjb1 failure. The node starts with a dead pair; the group's
// newer valid record is already in the catalogue (or arrives while Init runs).
// Init gets the 401 while the storage is not yet registered in op's storage
// map, so every event aimed at the mount finds nothing. The record the node
// already holds must still end up on the mount.
func TestReconcileStartupRaceAdoptsHeldRecord(t *testing.T) {
	for _, tc := range []struct {
		name         string
		heldBeforeLd bool // record is in the catalogue before the storage loads
	}{
		{"held before the storage loads", true},
		{"arrives during Init", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reconcileDB(t)
			m, err := Init(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := m.cfgStore.save(Config{Enabled: true, Key: "test-key", ApplyRemote: true}); err != nil {
				t.Fatal(err)
			}
			var probes atomic.Int32
			m.probeCredentialFn = func(_ context.Context, _ model.Storage, addition string) error {
				probes.Add(1)
				if strings.Contains(addition, `"v362"`) {
					return nil
				}
				return &sdk.Error{Code: sdk.CodeRefreshTokenError, Message: "dead"}
			}
			t.Cleanup(m.Stop)

			peer, _ := newIdentity()
			admin, _ := newIdentity()
			mount := fmt.Sprintf("/rc-t1-%d", reconcileSeq.Add(1))
			m.state.setGroups(admin, []group{{
				ID: "g1",
				Members: []member{
					{NodeID: m.id.NodeID, MountPath: mount},
					{NodeID: peer.NodeID, MountPath: "/peer"},
				},
			}}, 1)
			rec := signedRecord(peer, "/peer", "v362", 362)

			// Startup order: Start runs while no storage is loaded yet.
			m.Start()
			if tc.heldBeforeLd {
				if merged := m.absorbCreds([]*credRecord{rec}); len(merged) != 1 {
					t.Fatalf("record not merged: %v", merged)
				}
			}

			gate := make(chan struct{})
			setReconcileGate(gate)
			t.Cleanup(func() { setReconcileGate(nil) })
			storage := model.Storage{
				Driver:    "ReconcileFake",
				MountPath: mount,
				Addition:  `{"access_token":"expired","refresh_token":"old"}`,
				Modified:  time.Now().UTC().Truncate(time.Second),
			}
			if err := db.CreateStorage(&storage); err != nil {
				t.Fatal(err)
			}
			loaded := make(chan struct{})
			go func() {
				defer close(loaded)
				_ = op.LoadStorage(context.Background(), storage) // returns the Init error
			}()
			time.Sleep(200 * time.Millisecond) // inside Init, before registration
			if op.HasStorage(mount) {
				t.Fatal("test premise: the storage must not be registered while Init runs")
			}
			if !tc.heldBeforeLd {
				if merged := m.absorbCreds([]*credRecord{rec}); len(merged) != 1 {
					t.Fatalf("record not merged: %v", merged)
				}
			}
			close(gate)
			<-loaded
			t.Cleanup(func() {
				if s, ok := rowStatus(mount); ok {
					_ = op.DeleteStorageById(context.Background(), s.ID)
				}
			})

			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				if s, ok := rowStatus(mount); ok && s.Status == op.WORK && strings.Contains(s.Addition, "v362") {
					if probes.Load() == 0 {
						t.Fatal("storage recovered without probing the held record")
					}
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			s, _ := rowStatus(mount)
			t.Fatalf("held record v362 never reached the mount: status=%q addition=%q probes=%d", s.Status, s.Addition, probes.Load())
		})
	}
}

// T2: a push was lost. The catalogue holds a newer record but no event ever
// told the mount; only the periodic resync can notice. The mount is WORK on an
// older catalogued pair.
func TestReconcileResyncAppliesRecordThatNoEventAnnounced(t *testing.T) {
	reconcileDB(t)
	m, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// resync_sec is read by builds that have the knob; older ones ignore it.
	var cfg Config
	if err := json.Unmarshal([]byte(`{"enabled":true,"key":"test-key","apply_remote":true,"resync_sec":1}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.cfgStore.save(cfg); err != nil {
		t.Fatal(err)
	}
	m.probeCredentialFn = func(context.Context, model.Storage, string) error { return nil }
	t.Cleanup(m.Stop)

	peer, _ := newIdentity()
	admin, _ := newIdentity()
	mount := fmt.Sprintf("/rc-t2-%d", reconcileSeq.Add(1))
	m.state.setGroups(admin, []group{{
		ID: "g1",
		Members: []member{
			{NodeID: m.id.NodeID, MountPath: mount},
			{NodeID: peer.NodeID, MountPath: "/peer"},
		},
	}}, 1)
	id, err := op.CreateStorage(context.Background(), model.Storage{
		Driver:    "ReconcileFake",
		MountPath: mount,
		Addition:  `{"access_token":"v1","refresh_token":"r"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	old := signedRecord(peer, "/peer", "v1", 1)
	if !m.state.mergeCred(old) {
		t.Fatal("old record not merged")
	}
	m.Start()
	time.Sleep(500 * time.Millisecond)

	// The newer record reaches the catalogue without any event: the push that
	// would have carried it was dropped, and the catalogue got it another way.
	newer := signedRecord(peer, "/peer", "v2", 2)
	if !m.state.mergeCred(newer) {
		t.Fatal("newer record not merged")
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := rowStatus(mount); ok && strings.Contains(s.Addition, `"v2"`) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s, _ := rowStatus(mount)
	t.Fatalf("catalogued v2 never reached the mount: %q", s.Addition)
}

func localWithInvalidMount(t *testing.T, tag string) (mount string, id uint) {
	t.Helper()
	mount = fmt.Sprintf("/rc-local-%s-%d", tag, reconcileSeq.Add(1))
	var err error
	id, err = op.CreateStorage(context.Background(), model.Storage{
		Driver:    "Local",
		MountPath: mount,
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	d, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatal(err)
	}
	d.GetStorage().SetStatus("token invalid")
	return mount, id
}

func literalManager(t *testing.T, mount string, peers []*identity, probe func(context.Context, model.Storage, string) error) *Manager {
	t.Helper()
	local, _ := newIdentity()
	admin, _ := newIdentity()
	state := newStore()
	members := []member{{NodeID: local.NodeID, MountPath: mount}}
	for i, p := range peers {
		members = append(members, member{NodeID: p.NodeID, MountPath: fmt.Sprintf("/peer%d", i)})
	}
	state.setGroups(admin, []group{{ID: "g1", Members: members}}, 1)
	m := &Manager{
		dir:               t.TempDir(),
		id:                local,
		state:             state,
		conns:             newConnRegistry(),
		cfgStore:          &configStore{cfg: Config{Enabled: true, Key: "test-key", ApplyRemote: true}},
		stopCh:            make(chan struct{}),
		probeCredentialFn: probe,
	}
	t.Cleanup(m.Stop)
	return m
}

func localRecord(peer *identity, mount, token string, version uint64) *credRecord {
	r := signedRecord(peer, mount, token, version)
	r.OriginDriver = "Local"
	r.Sig = peer.sign(r.signingBytes())
	r.MountSig = peer.sign(r.mountSigningBytes())
	return r
}

func tokenHash(token string) string {
	return credHash(map[string]json.RawMessage{"access_token": json.RawMessage(fmt.Sprintf("%q", token))})
}

// T4a: of two catalogued candidates the newest is tried first. Its terminal 401
// is revoked and persisted before the next candidate is touched.
func TestReconcileNewestFirstAndRevocationPersistedBeforeNext(t *testing.T) {
	reconcileDB(t)
	mount, _ := localWithInvalidMount(t, "order")
	p1, _ := newIdentity()
	p2, _ := newIdentity()

	// The older candidate gets the lexicographically smaller hash, so an
	// implementation that orders by hash would try it first.
	var hiTok, loTok string
	for i := 0; ; i++ {
		hi, lo := fmt.Sprintf("hi-%d", i), fmt.Sprintf("lo-%d", i)
		if tokenHash(lo) < tokenHash(hi) {
			hiTok, loTok = hi, lo
			break
		}
	}
	var mu sync.Mutex
	var order []string
	var revokedBeforeSecond bool
	var m *Manager
	m = literalManager(t, mount, []*identity{p1, p2}, func(_ context.Context, _ model.Storage, addition string) error {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(addition, hiTok):
			order = append(order, "hi")
			return &sdk.Error{Code: sdk.CodeRefreshTokenError, Message: "dead"}
		case strings.Contains(addition, loTok):
			order = append(order, "lo")
			b, _ := os.ReadFile(m.statePath())
			revokedBeforeSecond = strings.Contains(string(b), tokenHash(hiTok))
			return nil
		}
		return errors.New("unexpected candidate")
	})
	lo := localRecord(p2, "/peer1", loTok, 5)
	hi := localRecord(p1, "/peer0", hiTok, 10)
	if merged := m.absorbCreds([]*credRecord{lo, hi}); len(merged) != 2 {
		t.Fatalf("merged %d, want 2", len(merged))
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m.state.candidateHealthy("g1", tokenHash(loTok), now()) {
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(order, ",") != "hi,lo" {
				t.Fatalf("probe order = %v, want [hi lo]", order)
			}
			if !revokedBeforeSecond {
				t.Fatal("the 401 revocation was not persisted before the next candidate was probed")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("mount did not recover; probe order %v", order)
}

// T4b: a probe failing for a reason that is neither a 401 nor the refresh
// throttle (network down, provider 5xx) must not lose the candidate.
func TestReconcileTransientProbeErrorIsRetried(t *testing.T) {
	reconcileDB(t)
	mount, _ := localWithInvalidMount(t, "transient")
	peer, _ := newIdentity()
	var probes atomic.Int32
	m := literalManager(t, mount, []*identity{peer}, func(context.Context, model.Storage, string) error {
		if probes.Add(1) < 3 {
			return errors.New("connection reset by peer")
		}
		return nil
	})
	if merged := m.absorbCreds([]*credRecord{localRecord(peer, "/peer0", "tok", 1)}); len(merged) != 1 {
		t.Fatal("not merged")
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if m.state.candidateHealthy("g1", tokenHash("tok"), now()) {
			if n := len(m.state.revocationSnapshot()); n != 0 {
				t.Fatalf("a transient error produced %d revocation(s)", n)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("candidate was dropped after a transient probe error (probes=%d)", probes.Load())
}

// T5: an event storm. 100 newer records for the same source arrive while the
// probe is slow and failing. Probes never overlap on the mount, and the work
// collapses to the current state: superseded records are not probed.
func TestReconcileEventStormCollapsesToLatestState(t *testing.T) {
	reconcileDB(t)
	mount, _ := localWithInvalidMount(t, "storm")
	peer, _ := newIdentity()
	var inFlight, maxInFlight, stale atomic.Int32
	const last = 100
	finalTok := fmt.Sprintf("tok-%d", last)
	m := literalManager(t, mount, []*identity{peer}, func(_ context.Context, _ model.Storage, addition string) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			cur := maxInFlight.Load()
			if n <= cur || maxInFlight.CompareAndSwap(cur, n) {
				break
			}
		}
		if !strings.Contains(addition, finalTok) {
			stale.Add(1)
		}
		time.Sleep(10 * time.Millisecond)
		return errors.New("provider unreachable")
	})
	for v := 1; v <= last; v++ {
		m.absorbCreds([]*credRecord{localRecord(peer, "/peer0", fmt.Sprintf("tok-%d", v), uint64(v))})
	}
	time.Sleep(2 * time.Second)
	if got := maxInFlight.Load(); got > 1 {
		t.Fatalf("%d probes in flight on one mount, want at most 1", got)
	}
	if got := stale.Load(); got > 3 {
		t.Fatalf("%d probes were spent on superseded records, want the work to collapse to the latest", got)
	}
}
