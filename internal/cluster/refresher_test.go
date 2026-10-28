package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	sdk "github.com/OpenListTeam/115-sdk-go"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/115-sdk-go/emu115"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/115_open"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// refresherRig is a Manager wired to a REAL 115 Open driver whose SDK talks to a
// real local HTTP server emulating 115 passport + API (emu115). No mocks: the
// driver, SDK, op layer and cluster engine are the production code.
type refresherRig struct {
	t      *testing.T
	emu    *emu115.Server
	m      *Manager
	mount  string
	local  *identity
	peer   *identity
	srvURL string
	d      interface {
		List(context.Context, model.Obj, model.ListArgs) ([]model.Obj, error)
	}
}

func newRefresherRig(t *testing.T, ttl time.Duration, cfg Config) *refresherRig {
	t.Helper()
	emu := emu115.New(emu115.Config{AccessTTL: ttl})
	srv := httptest.NewServer(emu.Handler())
	t.Cleanup(srv.Close)
	t.Setenv("OPENLIST_TEST_115_API_BASE", srv.URL)
	t.Setenv("OPENLIST_TEST_115_PASSPORT_BASE", srv.URL)

	dbName := fmt.Sprintf("file:cluster-refresher-%d?mode=memory&cache=shared", time.Now().UnixNano())
	database, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)

	access, refresh := emu.Tokens()
	mount := fmt.Sprintf("/refresher-%d", time.Now().UnixNano())
	add, _ := json.Marshal(map[string]any{"access_token": access, "refresh_token": refresh, "root_folder_id": "0", "limit_rate": 1000, "page_size": 100})
	id, err := op.CreateStorage(context.Background(), model.Storage{Driver: "115 Open", MountPath: mount, Addition: string(add)})
	if err != nil {
		t.Fatalf("create 115 Open storage against the emulator: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })

	local, _ := newIdentity()
	peer, _ := newIdentity()
	admin, _ := newIdentity()
	state := newStore()
	state.setGroups(admin, []group{{ID: "g1", Members: []member{
		{NodeID: local.NodeID, MountPath: mount},
		{NodeID: peer.NodeID, MountPath: "/peer"},
	}}}, 1)
	// The peer is online and runs the same driver.
	state.mergeInventory(&nodeInfo{NodeID: peer.NodeID, Label: "peer", Version: 1,
		Storages: []storageInfo{{MountPath: "/peer", Driver: "115 Open", Status: op.WORK}}}, now())
	if cfg.Key == "" {
		cfg.Key = "test-key"
	}
	cfg.Enabled, cfg.ApplyRemote = true, true
	m := &Manager{
		dir:      t.TempDir(),
		id:       local,
		state:    state,
		conns:    newConnRegistry(),
		dials:    map[string]*dialState{},
		cfgStore: &configStore{cfg: cfg},
		stopCh:   make(chan struct{}),
	}
	m.refr.ensureLocked()
	m.refr.startedAt = time.Now().Add(-time.Hour)
	op.SetRefreshCoordinator(m)
	t.Cleanup(func() { op.SetRefreshCoordinator(nil); close(m.stopCh) })

	d, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatal(err)
	}
	return &refresherRig{t: t, emu: emu, m: m, mount: mount, local: local, peer: peer, d: d, srvURL: srv.URL}
}

func (r *refresherRig) list() error {
	_, err := r.d.List(context.Background(), &model.Object{ID: "0", Name: "root"}, model.ListArgs{})
	return err
}

func (r *refresherRig) becomeHolder() {
	r.m.state.issueLease(r.local, "g1", 1, uint64(time.Now().Unix()), 3600, nil, time.Now())
}

func (r *refresherRig) peerHoldsLease() {
	l := r.m.state.issueLease(r.peer, "g1", 1, uint64(time.Now().Unix()), 3600, nil, time.Now())
	_ = l
}

// publishValid mimics the token-valid notification the driver raises after a
// refreshed pair is proven, which is what shares the pair with the cluster.
func (r *refresherRig) publishValid() {
	d, _ := op.GetStorageByMountPath(r.mount)
	st := d.GetStorage()
	r.m.onStorageCredential("token-valid", op.StorageCredentialEvent{Storage: d, Addition: st.Addition, Modified: st.Modified})
}

func TestNonRefresherNeverCallsPassportAndAsksForANewPair(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{})
	r.peerHoldsLease()
	if r.m.MayRefresh(r.mount) {
		t.Fatal("a member that is not the lease holder must not refresh")
	}
	r.emu.ExpireAccess()
	err := r.list()
	if err == nil || !strings.Contains(err.Error(), "40140125") {
		t.Fatalf("a non-refresher keeps the API error, got %v", err)
	}
	for i := 0; i < 5; i++ {
		_ = r.list() // a player retrying
	}
	if st := r.emu.Stats(); st.RefreshCalls != 0 {
		t.Fatalf("a non-refresher called passport %d times", st.RefreshCalls)
	}
	// It must not revoke the pair: an expired access token is not a dead chain.
	if n := len(r.m.state.revocationSnapshot()); n != 0 {
		t.Fatalf("an expired access token on a non-refresher revoked the pair cluster-wide (%d revocations)", n)
	}
	d, _ := op.GetStorageByMountPath(r.mount)
	if s := d.GetStorage().Status; s != op.WORK {
		t.Fatalf("mount must stay WORK while the new pair is fetched, status=%q", s)
	}
	r.m.refr.mu.Lock()
	p := r.m.refr.pending["g1\x00"+r.mount]
	r.m.refr.mu.Unlock()
	if p == nil {
		t.Fatal("the rejection must start the pull / refresh-request timer")
	}
}

func TestHolderRefreshesTransparentlyAndPublishesWithLifetime(t *testing.T) {
	r := newRefresherRig(t, 90*time.Minute, Config{})
	r.becomeHolder()
	if !r.m.MayRefresh(r.mount) {
		t.Fatal("holder must be allowed")
	}
	r.emu.ExpireAccess()
	if err := r.list(); err != nil {
		t.Fatalf("holder should refresh transparently: %v", err)
	}
	st := r.emu.Stats()
	if st.RefreshCalls != 1 || st.RefreshOK != 1 {
		t.Fatalf("want exactly one refresh, got %+v", st)
	}
	r.publishValid()
	rec, ok := r.m.state.getCredForSource("g1", r.local.NodeID, r.mount)
	if !ok {
		t.Fatal("the refreshed pair was not catalogued for sharing")
	}
	if rec.ExpiresIn != 5400 || rec.ObtainedAt == 0 || time.Now().Unix()-rec.ObtainedAt > 5 {
		t.Fatalf("shared record must carry the token lifetime (expires_in=%d obtained=%d)", rec.ExpiresIn, rec.ObtainedAt)
	}
	// The holder's summary reaches the lease and the activity log.
	r.m.renewLease(group{ID: "g1"})
	if l := r.m.state.leaseOf("g1").lease.Last; l == nil || l.Outcome != "ok" || l.ExpiresIn != 5400 {
		t.Fatalf("lease must carry the last refresh summary: %#v", l)
	}
}

func TestProactiveRefreshAtEightyPercentAndNeverBlindly(t *testing.T) {
	r := newRefresherRig(t, 10*time.Second, Config{ProactiveMinGapSec: 1})
	r.becomeHolder()
	// Expiry is unknown for the pair the mount started with: no blind refresh.
	r.m.refresherTick()
	time.Sleep(100 * time.Millisecond)
	if st := r.emu.Stats(); st.RefreshCalls != 0 {
		t.Fatalf("refreshed blindly with unknown expiry (%d calls)", st.RefreshCalls)
	}
	// First expiry signal: a refresh yields expires_in = 10s.
	r.emu.ExpireAccess()
	if err := r.list(); err != nil {
		t.Fatal(err)
	}
	if st := r.emu.Stats(); st.RefreshCalls != 1 {
		t.Fatalf("calls=%d", st.RefreshCalls)
	}
	// Before 80% (8s) nothing happens, no matter how often the loop ticks.
	for i := 0; i < 10; i++ {
		r.m.refresherTick()
		time.Sleep(20 * time.Millisecond)
	}
	if st := r.emu.Stats(); st.RefreshCalls != 1 {
		t.Fatalf("refreshed before 80%% of the lifetime: %d calls", st.RefreshCalls)
	}
	// At 80% the holder refreshes once, ahead of expiry, and the member never sees a 401.
	waitFor(t, "proactive refresh", 12*time.Second, func() bool {
		r.m.refresherTick()
		return r.emu.Stats().RefreshOK >= 2
	})
	st := r.emu.Stats()
	if st.RefreshOK != 2 {
		t.Fatalf("want exactly one proactive refresh, got %+v", st)
	}
	before := st.APIRejected
	if err := r.list(); err != nil {
		t.Fatalf("after the proactive refresh the mount must work: %v", err)
	}
	if got := r.emu.Stats().APIRejected; got != before {
		t.Fatal("the proactive refresh happened too late: a request was rejected")
	}
	// And it does not churn: the new token is fresh, so the loop is quiet again.
	for i := 0; i < 20; i++ {
		r.m.refresherTick()
	}
	time.Sleep(200 * time.Millisecond)
	if got := r.emu.Stats().RefreshCalls; got != 2 {
		t.Fatalf("refresh churn: %d calls", got)
	}
	// The trigger is recorded as proactive.
	found := false
	for _, e := range r.m.eventList() {
		if e.Kind == "refresh" && strings.Contains(e.Detail, "trigger proactive") {
			found = true
		}
	}
	if !found {
		t.Fatal("proactive refresh not visible in the activity log")
	}
}

func TestProactiveRefreshDoesNotLoopOnDeadOrThrottledChain(t *testing.T) {
	r := newRefresherRig(t, 2*time.Second, Config{ProactiveMinGapSec: 1})
	r.becomeHolder()
	r.emu.ExpireAccess()
	if err := r.list(); err != nil { // learn the lifetime (2s)
		t.Fatal(err)
	}
	r.emu.Kill() // the chain dies server-side
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		r.m.refresherTick()
		time.Sleep(50 * time.Millisecond)
	}
	st := r.emu.Stats()
	// One refresh succeeded earlier; after the kill the SDK's dead latch stops
	// every further call after a single 40140120.
	if st.RefreshFail[emu115.CodeRefreshTokenError] > 1 {
		t.Fatalf("holder kept hammering passport with a dead chain: %+v", st.RefreshFail)
	}
}

func TestProactiveRefreshBacksOffAfterThrottle(t *testing.T) {
	r := newRefresherRig(t, 2*time.Second, Config{ProactiveMinGapSec: 1})
	r.becomeHolder()
	r.emu.SetRefreshThrottle(time.Hour)
	r.emu.ExpireAccess()
	if err := r.list(); err != nil { // first refresh ok, starts the throttle window
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		r.m.refresherTick()
		time.Sleep(50 * time.Millisecond)
	}
	st := r.emu.Stats()
	if st.RefreshFail[emu115.CodeRefreshFrequently] > 1 {
		t.Fatalf("40140117 must start the SDK cooldown, not a retry loop: %+v", st.RefreshFail)
	}
}

func TestRefreshRequestFromMemberRefreshesOnceAndIsRateLimited(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{ProactiveMinGapSec: 3600})
	r.becomeHolder()
	cur := testCred(t, r.local, "g1", r.mount, 5, "mine", now())
	r.m.state.mergeCred(cur)
	req := &refreshRequest{GroupID: "g1", Mount: "/peer", HaveHash: shortHash(cur.CredHash), HaveVersion: cur.Version}
	r.m.handleRefreshRequest(r.peer.NodeID, req)
	waitFor(t, "refresh for the request", 5*time.Second, func() bool { return r.emu.Stats().RefreshOK == 1 })
	r.m.handleRefreshRequest(r.peer.NodeID, req)
	r.m.handleRefreshRequest(r.peer.NodeID, req)
	time.Sleep(300 * time.Millisecond)
	if got := r.emu.Stats().RefreshCalls; got != 1 {
		t.Fatalf("a member could make the refresher spend its refresh_token %d times", got)
	}
	log := r.m.requestLog()
	if len(log) != 3 || log[2].Result != "refresh started" || !strings.Contains(log[0].Result, "rate limited") {
		t.Fatalf("request log wrong: %+v", log)
	}
	// An outsider is ignored.
	outsider, _ := newIdentity()
	r.m.handleRefreshRequest(outsider.NodeID, req)
	if last := r.m.requestLog()[0]; !strings.Contains(last.Result, "not a member") {
		t.Fatalf("outsider request must be ignored: %+v", last)
	}
	// A request when we are not the refresher is ignored too.
	r.peerHoldsLease()
	r.m.handleRefreshRequest(r.peer.NodeID, req)
	if last := r.m.requestLog()[0]; !strings.Contains(last.Result, "not the refresher") {
		t.Fatalf("non-holder must ignore requests: %+v", last)
	}
}

func TestRefreshRequestFromLaggingMemberJustResendsCurrentPair(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{})
	r.becomeHolder()
	cur := testCred(t, r.local, "g1", r.mount, 9, "mine", now())
	r.m.state.mergeCred(cur)
	link := &peerConn{out: newOutbox(), closed: make(chan struct{})}
	r.m.conns.add(link)
	r.m.conns.bind(link, r.peer.NodeID, r.local.NodeID)
	r.m.handleRefreshRequest(r.peer.NodeID, &refreshRequest{GroupID: "g1", Mount: "/peer", HaveHash: "stale", HaveVersion: 3})
	if got := r.emu.Stats().RefreshCalls; got != 0 {
		t.Fatalf("a requester that is merely behind must not cost a refresh (%d calls)", got)
	}
	msg := link.out.takePriority()
	if msg == nil || msg.To != r.peer.NodeID || len(msg.Creds) != 1 || msg.Creds[0].Version != 9 {
		t.Fatalf("the current pair must be re-sent directly to the requester: %#v", msg)
	}
}

func TestRejectedMemberAsksTheHolderOnlyAfterTheWaitAndOnlyIfNoNewerPair(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{RefreshRequestWaitSec: 1})
	r.peerHoldsLease()
	link := &peerConn{out: newOutbox(), closed: make(chan struct{})}
	r.m.conns.add(link)
	r.m.conns.bind(link, r.peer.NodeID, r.local.NodeID)

	r.emu.ExpireAccess()
	_ = r.list()
	r.m.requestTick()
	if m := drainDirected(link, msgRefreshRequest); m != nil {
		t.Fatal("requested before the wait elapsed")
	}
	time.Sleep(1100 * time.Millisecond)
	r.m.requestTick()
	m := drainDirected(link, msgRefreshRequest)
	if m == nil || m.To != r.peer.NodeID || m.Req == nil || m.Req.GroupID != "g1" || m.Req.Mount != r.mount {
		t.Fatalf("expected a refresh-request to the holder, got %#v", m)
	}
	// No immediate repeat.
	r.m.requestTick()
	if drainDirected(link, msgRefreshRequest) != nil {
		t.Fatal("requests must be spaced out")
	}

	// A newer pair arrives in time (the real refresher spent the refresh_token and
	// pushed the result): the pending request is dropped, never sent.
	r.m.refr.mu.Lock()
	r.m.refr.pending["g1\x00"+r.mount].deadline = time.Now().Add(time.Hour)
	r.m.refr.mu.Unlock()
	_, oldRefresh := r.emu.Tokens()
	refresher := sdk.New(sdk.WithRefreshToken(oldRefresh), sdk.WithPassportBaseURL(r.srvURL))
	if _, err := refresher.RefreshToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	acc, ref := r.emu.Tokens()
	d, _ := op.GetStorageByMountPath(r.mount)
	st := *d.GetStorage()
	st.Addition = strings.Replace(st.Addition, accessTokenOf(st.Addition), acc, 1)
	st.Addition = strings.Replace(st.Addition, "refresh_token\":\""+refreshTokenOf(st.Addition), "refresh_token\":\""+ref, 1)
	if err := op.UpdateStorage(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	r.m.requestTick() // notices the token changed and forgets the request
	r.m.refr.mu.Lock()
	left := len(r.m.refr.pending)
	r.m.refr.mu.Unlock()
	if left != 0 {
		t.Fatalf("a request for an already-replaced token must be forgotten, %d pending", left)
	}
	if drainDirected(link, msgRefreshRequest) != nil {
		t.Fatal("a request was sent although a newer pair had already been adopted")
	}
}

func refreshTokenOf(additionJSON string) string {
	var all map[string]json.RawMessage
	_ = json.Unmarshal([]byte(additionJSON), &all)
	var s string
	_ = json.Unmarshal(all["refresh_token"], &s)
	return s
}

func drainDirected(c *peerConn, typ string) *syncMessage {
	for m := c.out.takePriority(); m != nil; m = c.out.takePriority() {
		if m.Type == typ {
			return m
		}
	}
	return nil
}

func TestRecoversMountThatFailedInitOnExpiredAccessToken(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{ProactiveMinGapSec: 1})
	r.becomeHolder()
	r.emu.ExpireAccess()
	d, _ := op.GetStorageByMountPath(r.mount)
	d.GetStorage().SetStatus("code: 40140125, message: access_token invalid") // what a failed Init leaves
	r.m.refresherTick()
	waitFor(t, "recovery refresh", 5*time.Second, func() bool { return r.emu.Stats().RefreshOK == 1 })
	waitFor(t, "mount back to WORK", 5*time.Second, func() bool {
		cur, err := db.GetStorageByMountPath(r.mount)
		return err == nil && cur.Status == op.WORK
	})
}

func TestTakeoverPullsFirstThenClaimsAndOthersWait(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{LeaseTTLSec: 4}) // pull wait = 1s
	// A live link that never answers: the taker must wait for its reply before claiming.
	link := &peerConn{out: newOutbox(), closed: make(chan struct{})}
	link.lastRx.Store(time.Now().UnixNano()) // the path itself works (pongs arrive)
	r.m.conns.add(link)
	r.m.conns.bind(link, r.peer.NodeID, r.local.NodeID)

	g, _ := r.m.state.groupByID("g1")
	start := time.Now()
	r.m.leaseStep(g) // no lease yet: first sighting starts the vacancy clock
	r.m.refr.mu.Lock()
	r.m.refr.noLeaseSince["g1"] = time.Now().Add(-time.Minute)
	r.m.refr.mu.Unlock()
	r.m.leaseStep(g)
	time.Sleep(200 * time.Millisecond)
	if r.m.isHolder("g1") {
		t.Fatal("claimed the lease before pulling the latest credential")
	}
	// The pull went out first.
	sawPull := false
	for m := link.out.takeBulk(); m != nil; m = link.out.takeBulk() {
		if m.Type == "pull" && len(m.Wants) == 1 && m.Wants[0] == "g1" {
			sawPull = true
		}
	}
	if !sawPull {
		t.Fatal("takeover must pull the group before claiming")
	}
	// The peer's reply arrives (any frame after the pull): the claim follows promptly.
	link.lastRecv.Store(time.Now().UnixNano())
	waitFor(t, "claim after the reply", 3*time.Second, func() bool { return r.m.isHolder("g1") })
	if time.Since(start) > 3*time.Second {
		t.Fatalf("claim took %s", time.Since(start))
	}
	e := r.m.state.leaseOf("g1")
	if e.lease.Epoch != 1 || e.lease.Holder != r.local.NodeID {
		t.Fatalf("unexpected lease %#v", e.lease)
	}
	// The lease was broadcast.
	pri := link.out.takePriority()
	if pri == nil || len(pri.Leases) != 1 {
		t.Fatalf("new lease must be pushed to peers, got %#v", pri)
	}
	// Settle: a brand-new claim does not refresh for a moment, so a simultaneous
	// taker's dominating lease can arrive first.
	if r.m.MayRefresh(r.mount) {
		t.Fatal("a freshly claimed lease must settle before refreshing")
	}
	time.Sleep(settleDelay + 100*time.Millisecond)
	if !r.m.MayRefresh(r.mount) {
		t.Fatal("after settling the holder may refresh")
	}
}

func TestOnlyTheFirstInOrderTakesOverImmediatelyOthersStagger(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{LeaseTTLSec: 30})
	live := &peerConn{out: newOutbox(), closed: make(chan struct{})}
	live.lastRx.Store(time.Now().UnixNano())
	r.m.conns.add(live)
	r.m.conns.bind(live, r.peer.NodeID, r.local.NodeID)
	// Make the peer rank first: the admin prefers it and it is online with a working mount.
	r.m.state.setPolicy(r.local, "g1", r.peer.NodeID, now())
	g, _ := r.m.state.groupByID("g1")
	r.m.refr.mu.Lock()
	r.m.refr.noLeaseSince["g1"] = time.Now().Add(-3 * time.Second)
	r.m.refr.mu.Unlock()
	r.m.leaseStep(g)
	time.Sleep(100 * time.Millisecond)
	if r.m.isHolder("g1") {
		t.Fatal("rank-1 node claimed before its stagger elapsed (the preferred refresher gets the first chance)")
	}
	// Once the stagger (TTL/3 = 10s) has passed without a claim, the next in order goes.
	r.m.refr.mu.Lock()
	r.m.refr.noLeaseSince["g1"] = time.Now().Add(-11 * time.Second)
	r.m.refr.mu.Unlock()
	r.m.leaseStep(g)
	waitFor(t, "fallback takeover", 5*time.Second, func() bool { return r.m.isHolder("g1") })
}

func TestHolderHandsOverToPreferredRefresherWhenItIsBackOnline(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{LeaseTTLSec: 30})
	r.becomeHolder()
	r.m.refr.mu.Lock()
	r.m.refr.acquiredAt["g1"] = time.Now().Add(-time.Hour)
	r.m.refr.renewedAt["g1"] = time.Now()
	r.m.refr.mu.Unlock()
	g, _ := r.m.state.groupByID("g1")
	// No preference: it keeps the role.
	r.m.leaseStep(g)
	if !r.m.isHolder("g1") {
		t.Fatal("holder gave the role up without a preferred refresher")
	}
	r.m.state.setPolicy(r.local, "g1", r.peer.NodeID, now())
	r.m.leaseStep(g)
	if r.m.isHolder("g1") {
		t.Fatal("holder must release the lease to the preferred refresher once it is online and working")
	}
	if e := r.m.state.leaseOf("g1"); e.lease.TTLSec != 0 || e.lease.Holder != r.local.NodeID {
		t.Fatalf("release must be a signed TTL-0 lease from the holder: %#v", e.lease)
	}
}

func TestSetPreferredRefresherValidatesAndReplicates(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{})
	link := &peerConn{out: newOutbox(), closed: make(chan struct{})}
	r.m.conns.add(link)
	r.m.conns.bind(link, r.peer.NodeID, r.local.NodeID)
	if err := r.m.SetPreferredRefresher("nope", r.peer.NodeID); err == nil {
		t.Fatal("unknown group must be rejected")
	}
	if err := r.m.SetPreferredRefresher("g1", "not-a-member"); err == nil {
		t.Fatal("a non-member must be rejected")
	}
	if err := r.m.SetPreferredRefresher("g1", r.peer.NodeID); err != nil {
		t.Fatal(err)
	}
	pri := link.out.takePriority()
	if pri == nil || len(pri.Policies) != 1 || pri.Policies[0].Preferred != r.peer.NodeID {
		t.Fatalf("policy must be pushed: %#v", pri)
	}
	st := r.m.Status()
	if st.Groups[0].Preferred != r.peer.NodeID {
		t.Fatalf("status must show the preferred refresher: %+v", st.Groups[0])
	}
}

func TestStatusShowsRefresherLaggardsAndLinkHealth(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{})
	r.becomeHolder()
	cur := testCred(t, r.local, "g1", r.mount, 12, "cur", now())
	r.m.state.mergeCred(cur)
	// The peer holds an older pair (version 7).
	r.m.state.mergeInventory(&nodeInfo{NodeID: r.peer.NodeID, Label: "peer", Version: 2,
		Storages: []storageInfo{{MountPath: "/peer", Driver: "115 Open", Status: op.WORK, CredHash: "abc", CredVer: 7}}}, now())
	link := &peerConn{out: newOutbox(), closed: make(chan struct{}), since: now() - 5}
	link.rttNanos.Store(int64(42 * time.Millisecond))
	link.peerProto.Store(1)
	r.m.conns.add(link)
	r.m.conns.bind(link, r.peer.NodeID, r.local.NodeID)
	r.m.links.disconnected(&peerConn{closed: make(chan struct{}), nodeID: r.peer.NodeID, since: now() - 3}, reasonWriteTimeout)

	st := r.m.Status()
	g := st.Groups[0]
	if !g.RefreshGroup || g.Refresher == nil || !g.Refresher.Valid || !g.Refresher.Self {
		t.Fatalf("refresher/lease missing: %+v", g.Refresher)
	}
	var peer MemberView
	for _, mem := range g.Members {
		if mem.NodeID == r.peer.NodeID {
			peer = mem
		}
	}
	if peer.HeldVersion != 7 || !peer.Laggard {
		t.Fatalf("lagging member not flagged: %+v", peer)
	}
	if !g.Mixed || peer.Proto != 1 {
		t.Fatalf("a protocol-1 member must flag the group as mixed: mixed=%v proto=%d", g.Mixed, peer.Proto)
	}
	if len(st.Links) != 1 || !st.Links[0].Connected || st.Links[0].RTTMs != 42 || st.Links[0].Disconnects != 1 ||
		st.Links[0].LastReason != reasonWriteTimeout || st.Links[0].Flaps10m != 1 {
		t.Fatalf("link health wrong: %+v", st.Links)
	}
}

func TestMayRefreshKeepsLegacyBehaviourOutsideGroupsOrWithoutCluster(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{})
	if !r.m.MayRefresh("/not-in-any-group") {
		t.Fatal("a mount outside every group refreshes for itself")
	}
	r.m.cfgStore.cfg.ApplyRemote = false
	if !r.m.MayRefresh(r.mount) {
		t.Fatal("an observe-only node keeps the old behaviour")
	}
	r.m.cfgStore.cfg.ApplyRemote = true
	r.m.cfgStore.cfg.Enabled = false
	if !r.m.MayRefresh(r.mount) {
		t.Fatal("with the cluster disabled every node refreshes for itself")
	}
}

// A link that is up but silent is a stalled path, not a dead holder: claiming
// would put a stale refresh_token in play against a holder that is alive.
func TestStalledLinkBlocksTakeoverIsolatedNodeWaitsLonger(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{LeaseTTLSec: 30})
	g, _ := r.m.state.groupByID("g1")
	vacant := func(d time.Duration) {
		r.m.refr.mu.Lock()
		r.m.refr.noLeaseSince["g1"] = time.Now().Add(-d)
		r.m.refr.mu.Unlock()
	}

	// Link present, but nothing heard for longer than the TTL.
	stalled := &peerConn{out: newOutbox(), closed: make(chan struct{})}
	stalled.lastRx.Store(time.Now().Add(-time.Minute).UnixNano())
	r.m.conns.add(stalled)
	r.m.conns.bind(stalled, r.peer.NodeID, r.local.NodeID)
	vacant(time.Hour)
	r.m.leaseStep(g)
	time.Sleep(150 * time.Millisecond)
	if r.m.isHolder("g1") {
		t.Fatal("claimed the lease behind a silent link: the holder may be alive on the other side of a stall")
	}

	// The stalled link is declared dead; the node is now isolated. A short
	// vacancy is not enough...
	stalled.close("test")
	r.m.conns.remove(stalled) // (a bare test link has no manager to do it)
	vacant(40 * time.Second) // > TTL but < 3 x TTL
	r.m.leaseStep(g)
	time.Sleep(150 * time.Millisecond)
	if r.m.isHolder("g1") {
		t.Fatal("an isolated node claimed after only a TTL of silence")
	}
	// ...but a long one is: it may be the only survivor.
	vacant(100 * time.Second)
	r.m.leaseStep(g)
	waitFor(t, "isolated survivor to claim", 5*time.Second, func() bool { return r.m.isHolder("g1") })
}

// A relay keeps re-gossiping the last inventory entry of a node that died. That
// echo must not make the dead node look alive for the refresher election: the
// holder would hand the lease to it and leave the group without a refresher for
// a whole TTL, again and again.
func TestDeadPreferredRefresherEchoedByRelayIsNotHandedTheLease(t *testing.T) {
	r := newRefresherRig(t, time.Hour, Config{LeaseTTLSec: 30, AnnounceIntervalSec: 10})
	r.becomeHolder()
	r.m.refr.mu.Lock()
	r.m.refr.acquiredAt["g1"] = time.Now().Add(-time.Hour)
	r.m.refr.renewedAt["g1"] = time.Now()
	r.m.refr.mu.Unlock()
	r.m.state.setPolicy(r.local, "g1", r.peer.NodeID, now())
	g, _ := r.m.state.groupByID("g1")

	// The peer's own entry (Version 1) was last advanced 100 s ago; since then a
	// relay has re-sent the same version every few seconds.
	r.m.state.mu.Lock()
	r.m.state.inventory[r.peer.NodeID].advancedAt = now() - 100
	r.m.state.mu.Unlock()
	for i := 0; i < 5; i++ {
		r.m.state.mergeInventory(&nodeInfo{NodeID: r.peer.NodeID, Label: "peer", Version: 1,
			Storages: []storageInfo{{MountPath: "/peer", Driver: "115 Open", Status: op.WORK}}}, now())
	}
	if n, _ := r.m.nodeInfoOf(r.peer.NodeID); n.seenAt < now()-1 {
		t.Fatal("test premise: the echo does refresh seenAt")
	}
	r.m.leaseStep(g)
	if !r.m.isHolder("g1") {
		t.Fatal("the lease was handed to a preferred refresher that has been silent for 100 s")
	}

	// The peer really announces again (Version moves): now it is live.
	r.m.state.mergeInventory(&nodeInfo{NodeID: r.peer.NodeID, Label: "peer", Version: 2,
		Storages: []storageInfo{{MountPath: "/peer", Driver: "115 Open", Status: op.WORK}}}, now())
	r.m.leaseStep(g)
	if r.m.isHolder("g1") {
		t.Fatal("a preferred refresher whose own announces arrive must get the lease")
	}
}
