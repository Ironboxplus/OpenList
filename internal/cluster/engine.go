package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

const (
	// replayWindowSec bounds clock skew + in-flight time for envelope freshness.
	replayWindowSec = 300
	// syncPath is the single endpoint peers exchange sealed messages on.
	syncPath = "/api/cluster/sync"
)

// Manager is the running cluster-sync engine for this node.
type Manager struct {
	dir      string
	id       *identity
	cfgStore *configStore
	state    *store
	replay   *replayCache
	client   *http.Client

	persistMu sync.Mutex

	stopCh chan struct{}
	once   sync.Once
}

// Default is the process-wide manager, set by Init. It is nil when cluster sync
// was never initialized.
var Default *Manager

func now() int64 { return time.Now().Unix() }

// Init constructs the manager: loads identity + config + persisted CRDT state and
// registers the storage hook so local credential changes propagate. It does NOT
// start the network loops — call Start once storages are loaded. Safe to call
// even when the feature is disabled (it simply stays dormant).
func Init(dataDir string) (*Manager, error) {
	dir := filepath.Join(dataDir, "cluster")
	id, err := loadOrCreateIdentity(dir)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		dir:      dir,
		id:       id,
		cfgStore: newConfigStore(dir),
		state:    newStore(),
		replay:   newReplayCache(replayWindowSec),
		stopCh:   make(chan struct{}),
	}
	if _, err := m.cfgStore.loadOrInit(); err != nil {
		return nil, err
	}
	cfg := m.cfgStore.get()
	m.client = &http.Client{Timeout: time.Duration(cfg.requestTimeout()) * time.Second}
	m.loadState()

	op.RegisterStorageHook(m.onStorageHook)
	Default = m
	return m, nil
}

// NodeID returns this node's stable identity string.
func (m *Manager) NodeID() string { return m.id.NodeID }

// RecordView is a redaction-safe summary of one synced mount for the admin UI.
// It deliberately omits the Addition (which holds secrets/tokens).
type RecordView struct {
	MountPath string `json:"mount_path"`
	Driver    string `json:"driver"`
	Version   uint64 `json:"version"`
	Origin    string `json:"origin"`
	Tombstone bool   `json:"tombstone"`
	UpdatedAt int64  `json:"updated_at"`
	Self      bool   `json:"self"` // true if this node authored the current version
}

// Status is the admin overview of the cluster state.
type Status struct {
	NodeID  string       `json:"node_id"`
	Enabled bool         `json:"enabled"`
	Active  bool         `json:"active"`
	Peers   []string     `json:"peers"`
	Records []RecordView `json:"records"`
}

// Status returns a redaction-safe snapshot for the admin UI.
func (m *Manager) Status() Status {
	cfg := m.cfgStore.get()
	recs := m.state.snapshot()
	views := make([]RecordView, 0, len(recs))
	for _, r := range recs {
		views = append(views, RecordView{
			MountPath: r.MountPath,
			Driver:    r.Config.Driver,
			Version:   r.Version,
			Origin:    r.Origin,
			Tombstone: r.Tombstone,
			UpdatedAt: r.UpdatedAt,
			Self:      r.Origin == m.id.NodeID,
		})
	}
	return Status{
		NodeID:  m.id.NodeID,
		Enabled: cfg.Enabled,
		Active:  cfg.active(),
		Peers:   cfg.peerList(),
		Records: views,
	}
}

// ---- persistence ----

type persistedState struct {
	Lamport uint64    `json:"lamport"`
	Records []*record `json:"records"`
}

func (m *Manager) statePath() string { return filepath.Join(m.dir, "state.json") }

func (m *Manager) loadState() {
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		return
	}
	var ps persistedState
	if err := json.Unmarshal(b, &ps); err != nil {
		utils.Log.Warnf("[cluster] corrupt state file, ignoring: %v", err)
		return
	}
	m.state.load(ps.Records, ps.Lamport)
}

func (m *Manager) persist() {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	ps := persistedState{Lamport: m.state.lamportNow(), Records: m.state.snapshot()}
	b, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return
	}
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		utils.Log.Warnf("[cluster] failed to persist state: %v", err)
		return
	}
	_ = os.Rename(tmp, m.statePath())
}

// ---- config access ----

// GetConfig returns the current config (with the key redacted for display when
// redact is true).
func (m *Manager) GetConfig(redact bool) Config {
	c := m.cfgStore.get()
	if redact && c.Key != "" {
		c.Key = "********"
	}
	return c
}

// SetConfig persists a new config. A blank/"********" key keeps the existing key.
func (m *Manager) SetConfig(c Config) error {
	old := m.cfgStore.get()
	if c.Key == "" || c.Key == "********" {
		c.Key = old.Key
	}
	if err := m.cfgStore.save(c); err != nil {
		return err
	}
	m.client.Timeout = time.Duration(c.requestTimeout()) * time.Second
	// Seed records for any newly in-scope local storages so we announce them.
	go m.seedLocalStorages()
	return nil
}

// ---- lifecycle ----

// Start seeds records from local storages and launches the anti-entropy loop.
func (m *Manager) Start() {
	m.once.Do(func() {
		go m.seedLocalStorages()
		go m.announceLoop()
	})
}

// Stop halts background loops.
func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
}

// seedLocalStorages records the current healthy, in-scope local storages as CRDT
// records (idempotent: unchanged configs cause no version churn) so this node has
// something to announce/serve.
func (m *Manager) seedLocalStorages() {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		return
	}
	var changed bool
	for _, d := range op.GetAllStorages() {
		st := d.GetStorage()
		if st.Status != op.WORK || !cfg.shouldShare(st.Driver, st.MountPath) {
			continue
		}
		if _, ok := m.state.localChange(m.id, fromModel(st), now()); ok {
			changed = true
		}
	}
	if changed {
		m.persist()
		m.broadcast(&syncMessage{Type: "announce", Digests: m.state.digests()})
	}
}

// announceLoop periodically broadcasts our digest so peers can pull anything they
// missed (anti-entropy backstop for lost push messages).
func (m *Manager) announceLoop() {
	for {
		cfg := m.cfgStore.get()
		interval := time.Duration(cfg.announceInterval()) * time.Second
		select {
		case <-m.stopCh:
			return
		case <-time.After(interval):
		}
		cfg = m.cfgStore.get()
		if !cfg.active() {
			continue
		}
		m.broadcast(&syncMessage{Type: "announce", Digests: m.state.digests()})
	}
}

// ---- storage hook (local change source) ----

func (m *Manager) onStorageHook(typ string, d driver.Driver) {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		return
	}
	st := d.GetStorage()
	if !cfg.shouldShare(st.Driver, st.MountPath) {
		return
	}
	switch typ {
	case "add", "update":
		if st.Status != op.WORK {
			// Health gating: never propagate a broken/expired token. Instead try
			// to recover a good one from a peer.
			m.pullMount(st.MountPath)
			return
		}
		rec, ok := m.state.localChange(m.id, fromModel(st), now())
		if ok {
			m.persist()
			m.broadcast(&syncMessage{Type: "push", Records: []*record{rec}})
		}
	case "del":
		if !cfg.ShareDeletes {
			return
		}
		rec, ok := m.state.localDelete(m.id, st.MountPath, now())
		if ok {
			m.persist()
			m.broadcast(&syncMessage{Type: "push", Records: []*record{rec}})
		}
	case "token-invalid":
		// A driver reported its token is dead; pull a fresh one from peers.
		m.pullMount(st.MountPath)
	}
}

// pullMount asks every peer for the latest record of a single mount and applies
// the best healthy answer.
func (m *Manager) pullMount(mountPath string) {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		return
	}
	msg := &syncMessage{Type: "pull", Wants: []string{mountPath}}
	for _, peer := range cfg.peerList() {
		m.exchange(peer, msg)
	}
}

// ---- message processing (shared by client replies and server requests) ----

// process integrates an incoming message and returns the reply to send back.
// It is the heart of the anti-entropy algorithm.
func (m *Manager) process(in *syncMessage) *syncMessage {
	reply := &syncMessage{Type: "reply"}

	// 1. Absorb any full records offered to us.
	m.applyRecords(in.Records)

	// 2. Anti-entropy on digests: tell the peer where we differ.
	if len(in.Digests) > 0 {
		peerHas := make(map[string]digest, len(in.Digests))
		for _, dg := range in.Digests {
			peerHas[dg.MountPath] = dg
			local, ok := m.state.get(dg.MountPath)
			if !ok {
				// We lack it entirely — request the full record.
				reply.Wants = append(reply.Wants, dg.MountPath)
				continue
			}
			cmp := digestOf(local)
			switch {
			case digestDominates(cmp, dg):
				reply.Records = append(reply.Records, local) // ours is newer
			case digestDominates(dg, cmp):
				reply.Wants = append(reply.Wants, dg.MountPath) // theirs is newer
			}
		}
		// Records we hold that the peer never mentioned — it is missing them.
		for _, local := range m.state.snapshot() {
			if _, seen := peerHas[local.MountPath]; !seen {
				reply.Records = append(reply.Records, local)
			}
		}
	}

	// 3. Serve explicit wants (pull).
	for _, mp := range in.Wants {
		if r, ok := m.state.get(mp); ok {
			reply.Records = append(reply.Records, r)
		}
	}
	return reply
}

// applyRecords verifies, merges and (if configured) applies a batch of records.
func (m *Manager) applyRecords(recs []*record) {
	if len(recs) == 0 {
		return
	}
	cfg := m.cfgStore.get()
	var dirty bool
	for _, r := range recs {
		if r == nil || !r.verify() {
			continue
		}
		switch m.state.merge(r) {
		case mergeApplied:
			dirty = true
			if cfg.ApplyRemote {
				m.applyToLocal(r)
			}
		case mergeTombstone:
			dirty = true
			if cfg.ApplyRemote {
				m.deleteLocal(r.MountPath)
			}
		}
	}
	if dirty {
		m.persist()
	}
}

// applyToLocal creates or updates the local storage from a record's config. The
// resulting op hook is neutralized by content-hash idempotency: state already
// holds this exact hash, so onStorageHook's localChange is a no-op.
func (m *Manager) applyToLocal(r *record) {
	ctx := context.Background()
	if d, err := op.GetStorageByMountPath(r.MountPath); err == nil {
		existing := *d.GetStorage() // copy; preserve ID/Status
		r.Config.applyTo(&existing)
		if err := op.UpdateStorage(ctx, existing); err != nil {
			utils.Log.Warnf("[cluster] apply update %s failed: %v", r.MountPath, err)
		} else {
			utils.Log.Infof("[cluster] applied peer config for %s (v%d from %s)", r.MountPath, r.Version, r.Origin)
		}
		return
	}
	var st model.Storage
	r.Config.applyTo(&st)
	if _, err := op.CreateStorage(ctx, st); err != nil {
		utils.Log.Warnf("[cluster] apply create %s failed: %v", r.MountPath, err)
	} else {
		utils.Log.Infof("[cluster] created storage %s from peer (v%d from %s)", r.MountPath, r.Version, r.Origin)
	}
}

func (m *Manager) deleteLocal(mountPath string) {
	d, err := op.GetStorageByMountPath(mountPath)
	if err != nil {
		return
	}
	id := d.GetStorage().ID
	if err := op.DeleteStorageById(context.Background(), id); err != nil {
		utils.Log.Warnf("[cluster] apply delete %s failed: %v", mountPath, err)
	}
}

// ---- networking ----

// broadcast sends a message to every peer (fire-and-forget), processing each
// peer's reply.
func (m *Manager) broadcast(msg *syncMessage) {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		return
	}
	for _, peer := range cfg.peerList() {
		go m.exchange(peer, msg)
	}
}

// exchange performs one full round-trip with a single peer: send msg, absorb the
// reply's records, and satisfy any records the peer asked for (reply.Wants) with
// an immediate follow-up push to that same peer.
func (m *Manager) exchange(peer string, msg *syncMessage) {
	reply, err := m.send(peer, msg)
	if err != nil {
		utils.Log.Debugf("[cluster] send to %s failed: %v", peer, err)
		return
	}
	m.applyRecords(reply.Records)
	if len(reply.Wants) > 0 {
		out := &syncMessage{Type: "push"}
		for _, mp := range reply.Wants {
			if r, ok := m.state.get(mp); ok {
				out.Records = append(out.Records, r)
			}
		}
		if len(out.Records) > 0 {
			if _, err := m.send(peer, out); err != nil {
				utils.Log.Debugf("[cluster] follow-up push to %s failed: %v", peer, err)
			}
		}
	}
}

// send seals msg, POSTs it to a peer's sync endpoint, and returns the decrypted
// reply.
func (m *Manager) send(peer string, msg *syncMessage) (*syncMessage, error) {
	cfg := m.cfgStore.get()
	key, err := deriveAEADKey([]byte(cfg.Key))
	if err != nil {
		return nil, err
	}
	body, err := sealEnvelope(key, m.id, msg, now())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, peer+syncPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{code: resp.StatusCode, body: string(raw)}
	}
	_, reply, err := openEnvelope(key, raw, now(), m.replay, replayWindowSec)
	if err != nil {
		return nil, err
	}
	return reply, nil
}

// HandleSync is the HTTP handler body: it decrypts a peer's request, processes
// it, and writes back a sealed reply. Returns the sealed reply bytes and any
// error (caller maps error to an HTTP status).
func (m *Manager) HandleSync(raw []byte) ([]byte, error) {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		return nil, errDisabled
	}
	key, err := deriveAEADKey([]byte(cfg.Key))
	if err != nil {
		return nil, err
	}
	_, msg, err := openEnvelope(key, raw, now(), m.replay, replayWindowSec)
	if err != nil {
		return nil, err
	}
	reply := m.process(msg)
	return sealEnvelope(key, m.id, reply, now())
}

type httpError struct {
	code int
	body string
}

func (e *httpError) Error() string { return e.body }

type sentinel string

func (s sentinel) Error() string { return string(s) }

const errDisabled = sentinel("cluster sync disabled")

// ---- digest helpers ----

func digestOf(r *record) digest {
	return digest{MountPath: r.MountPath, ContentHash: r.ContentHash, Version: r.Version, Origin: r.Origin, Tombstone: r.Tombstone}
}

// digestDominates reports whether a wins over b under the same LWW ordering as
// records.
func digestDominates(a, b digest) bool {
	if a.Version != b.Version {
		return a.Version > b.Version
	}
	if a.Origin != b.Origin {
		return a.Origin > b.Origin
	}
	return a.ContentHash > b.ContentHash
}
