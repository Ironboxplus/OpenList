package cluster

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// testNode is a full Manager with a real WebSocket endpoint, so these tests
// exercise the real transport (AEAD frames over gorilla/websocket), not mocks.
type testNode struct {
	m   *Manager
	srv *httptest.Server
}

func newTestNode(t *testing.T, label, key string) *testNode {
	t.Helper()
	id, err := newIdentity()
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		dir:      t.TempDir(),
		id:       id,
		cfgStore: &configStore{cfg: Config{Enabled: true, Key: key, Label: label, ApplyRemote: false, AnnounceIntervalSec: 1}},
		state:    newStore(),
		replay:   newReplayCache(replayWindowSec),
		conns:    newConnRegistry(),
		dials:    make(map[string]*dialState),
		stopCh:   make(chan struct{}),
	}
	srv := httptest.NewServer(http.HandlerFunc(m.ServeWS))
	m.cfgStore.cfg.Addr = srv.URL
	t.Cleanup(func() {
		m.Stop()
		srv.Close()
	})
	return &testNode{m: m, srv: srv}
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// shareGroups installs the same signed group document on every node.
func shareGroups(nodes []*testNode, groups []group) {
	admin, _ := newIdentity()
	d := nodes[0].m.state.setGroups(admin, groups, 1)
	for _, n := range nodes[1:] {
		n.m.state.mergeGroups(&d)
	}
}

func TestLinkBurstNeverDropsConnectionAndConverges(t *testing.T) {
	a := newTestNode(t, "a", "k")
	b := newTestNode(t, "b", "k")
	var members []member
	const mounts = 40
	for i := 0; i < mounts; i++ {
		members = append(members, member{NodeID: a.m.id.NodeID, MountPath: fmt.Sprintf("/m%d", i)})
	}
	members = append(members, member{NodeID: b.m.id.NodeID, MountPath: "/b"})
	shareGroups([]*testNode{a, b}, []group{{ID: "g1", Members: members}})

	a.m.ensureDial(b.srv.URL)
	waitFor(t, "link up", 5*time.Second, func() bool { return a.m.conns.hasNode(b.m.id.NodeID) && b.m.conns.hasNode(a.m.id.NodeID) })
	link := a.m.conns.get(b.m.id.NodeID)

	// 40 sources x 200 versions each, pushed back to back: 8000 messages, which
	// the old 128-slot FIFO could not hold even for a moment.
	const versions = 200
	for v := 1; v <= versions; v++ {
		for i := 0; i < mounts; i++ {
			r := testCred(t, a.m.id, "g1", fmt.Sprintf("/m%d", i), uint64(v), fmt.Sprintf("tok-%d-%d", i, v), int64(v))
			a.m.state.mergeCred(r)
			a.m.broadcast(&syncMessage{Type: "push", Creds: []*credRecord{r}})
		}
	}
	waitFor(t, "B to converge on the final version of every source", 20*time.Second, func() bool {
		for i := 0; i < mounts; i++ {
			r, ok := b.m.state.getCredForSource("g1", a.m.id.NodeID, fmt.Sprintf("/m%d", i))
			if !ok || r.Version != versions {
				return false
			}
		}
		return true
	})
	if link.isClosed() {
		t.Fatalf("link was closed during the burst (reason %q)", link.closeReason())
	}
	if st, _ := a.m.links.snapshot(b.m.id.NodeID, 600); st.Disconnects != 0 {
		t.Fatalf("burst caused %d disconnects (last reason %q)", st.Disconnects, st.LastReason)
	}
	if got := a.m.conns.get(b.m.id.NodeID); got != link {
		t.Fatal("the link was replaced during the burst")
	}
}

func TestSimultaneousDialsLeaveExactlyOneLinkPerPair(t *testing.T) {
	a := newTestNode(t, "a", "k")
	b := newTestNode(t, "b", "k")
	a.m.ensureDial(b.srv.URL)
	b.m.ensureDial(a.srv.URL)
	waitFor(t, "a link on both ends", 5*time.Second, func() bool {
		return a.m.conns.hasNode(b.m.id.NodeID) && b.m.conns.hasNode(a.m.id.NodeID)
	})
	// Let the tie-break settle, then require one link each and that both ends kept the same one.
	waitFor(t, "duplicate resolved", 5*time.Second, func() bool {
		return len(a.m.conns.all()) == 1 && len(b.m.conns.all()) == 1
	})
	time.Sleep(300 * time.Millisecond)
	if len(a.m.conns.all()) != 1 || len(b.m.conns.all()) != 1 {
		t.Fatalf("links a=%d b=%d, want 1 each", len(a.m.conns.all()), len(b.m.conns.all()))
	}
	ca, cb := a.m.conns.get(b.m.id.NodeID), b.m.conns.get(a.m.id.NodeID)
	if ca.outbound == cb.outbound {
		t.Fatal("both ends must describe the same physical link (one dialed, one accepted)")
	}
	// The surviving link is the one dialed by the smaller node id.
	smallerIsA := a.m.id.NodeID < b.m.id.NodeID
	if smallerIsA != ca.outbound {
		t.Fatalf("survivor must be dialed by the smaller node id (a smaller=%v, a dialed it=%v)", smallerIsA, ca.outbound)
	}
	// And it must keep working after the dust settles.
	time.Sleep(500 * time.Millisecond)
	if ca.isClosed() || cb.isClosed() {
		t.Fatal("the surviving link was closed")
	}
	// A redial must not be attempted while the pair already has a link.
	a.m.ensureDial(b.srv.URL)
	b.m.ensureDial(a.srv.URL)
	time.Sleep(300 * time.Millisecond)
	if len(a.m.conns.all()) != 1 || len(b.m.conns.all()) != 1 {
		t.Fatal("ensureDial opened a second link to an already-connected node")
	}
}

func TestWrongKeyIsRejectedWithReason(t *testing.T) {
	a := newTestNode(t, "a", "key-one")
	b := newTestNode(t, "b", "key-two")
	a.m.ensureDial(b.srv.URL)
	waitFor(t, "rejection", 5*time.Second, func() bool { return len(a.m.conns.all()) == 0 })
	if a.m.conns.hasNode(b.m.id.NodeID) {
		t.Fatal("a node with the wrong key must not be admitted")
	}
	waitFor(t, "dial loop to record the failure", 5*time.Second, func() bool { d, _ := a.m.dialStateFor(b.srv.URL); return !d })
	if _, backoff := a.m.dialStateFor(b.srv.URL); backoff < dialBackoffMin {
		t.Fatalf("a link that died at once must grow the redial backoff, got %s", backoff)
	}
}

// Acks and refresh-requests are directed: a hub that is not the target forwards
// them, so NAT'd members that only reach each other through it still work.
func TestDirectedAckAndRelayedPushConfirmDeliveryThroughHub(t *testing.T) {
	hub := newTestNode(t, "hub", "k")
	a := newTestNode(t, "a", "k")
	b := newTestNode(t, "b", "k")
	shareGroups([]*testNode{hub, a, b}, []group{{ID: "g1", Members: []member{
		{NodeID: hub.m.id.NodeID, MountPath: "/h"},
		{NodeID: a.m.id.NodeID, MountPath: "/a"},
		{NodeID: b.m.id.NodeID, MountPath: "/b"},
	}}})
	a.m.cfgStore.cfg.Addr, b.m.cfgStore.cfg.Addr = "", "" // behind NAT: only the hub is dialable
	a.m.ensureDial(hub.srv.URL)
	b.m.ensureDial(hub.srv.URL)
	waitFor(t, "star topology", 5*time.Second, func() bool {
		return hub.m.conns.hasNode(a.m.id.NodeID) && hub.m.conns.hasNode(b.m.id.NodeID) &&
			a.m.conns.hasNode(hub.m.id.NodeID) && b.m.conns.hasNode(hub.m.id.NodeID)
	})
	if a.m.conns.hasNode(b.m.id.NodeID) {
		t.Fatal("test premise: a and b have no direct link")
	}

	// The hub must be able to relay, so it has to be willing to offer the record
	// (a relay forwards authorised, verified candidates).
	r := testCred(t, a.m.id, "g1", "/a", 7, "tok", time.Now().Unix())
	a.m.state.mergeCred(r)
	a.m.trackDelivery(r)
	a.m.broadcast(&syncMessage{Type: "push", Creds: []*credRecord{r}})

	waitFor(t, "b to receive the record through the hub", 5*time.Second, func() bool {
		got, ok := b.m.state.getCredForSource("g1", a.m.id.NodeID, "/a")
		return ok && got.Version == 7
	})
	// b acks to a (directed, forwarded by the hub).
	waitFor(t, "a to see b's ack", 5*time.Second, func() bool {
		c, _ := a.m.deliveryConfirmed(credDigestKey(digestOfCred(r)))
		return c[b.m.id.NodeID]
	})
}

func TestUnconfirmedDeliveryIsResentDirectly(t *testing.T) {
	a := newTestNode(t, "a", "k")
	b := newTestNode(t, "b", "k")
	shareGroups([]*testNode{a, b}, []group{{ID: "g1", Members: []member{
		{NodeID: a.m.id.NodeID, MountPath: "/a"}, {NodeID: b.m.id.NodeID, MountPath: "/b"},
	}}})
	a.m.cfgStore.cfg.DeliveryResendSec = 1
	a.m.ensureDial(b.srv.URL)
	waitFor(t, "link", 5*time.Second, func() bool { return a.m.conns.hasNode(b.m.id.NodeID) && b.m.conns.hasNode(a.m.id.NodeID) })
	// b is known online to a via the hello it sent.
	waitFor(t, "b in a's inventory", 5*time.Second, func() bool { _, ok := a.m.nodeInfoOf(b.m.id.NodeID); return ok })

	r := testCred(t, a.m.id, "g1", "/a", 3, "tok", time.Now().Unix())
	a.m.state.mergeCred(r)
	a.m.trackDelivery(r) // tracked, but the push below is "lost": never sent
	if c, _ := a.m.deliveryConfirmed(credDigestKey(digestOfCred(r))); c[b.m.id.NodeID] {
		t.Fatal("premise: nothing confirmed yet")
	}
	time.Sleep(1100 * time.Millisecond)
	a.m.deliveryTick() // past the resend delay: direct resend to b
	waitFor(t, "b to receive the resent record", 5*time.Second, func() bool {
		got, ok := b.m.state.getCredForSource("g1", a.m.id.NodeID, "/a")
		return ok && got.Version == 3
	})
	waitFor(t, "confirmation after the resend", 5*time.Second, func() bool {
		c, _ := a.m.deliveryConfirmed(credDigestKey(digestOfCred(r)))
		return c[b.m.id.NodeID]
	})
	a.m.deliveryTick()
	if _, still := a.m.deliveryConfirmed(credDigestKey(digestOfCred(r))); still {
		t.Fatal("a fully confirmed delivery must be retired")
	}
}

func TestRevocationDigestSyncPullsOnlyOnMismatch(t *testing.T) {
	a := newTestNode(t, "a", "k")
	b := newTestNode(t, "b", "k")
	shareGroups([]*testNode{a, b}, []group{{ID: "g1", Members: []member{
		{NodeID: a.m.id.NodeID, MountPath: "/a"}, {NodeID: b.m.id.NodeID, MountPath: "/b"},
	}}})
	// a holds a revocation b does not: only a digest travels, then b pulls the set.
	dead := testCred(t, a.m.id, "g1", "/a", 1, "dead", 1)
	a.m.state.mergeCred(dead)
	if _, ok := a.m.state.revokePair(a.m.id, "g1", "/a", dead.CredHash, 2); !ok {
		t.Fatal("revoke")
	}
	ann := a.m.announceMessage()
	if len(ann.Revocations) != 0 || ann.RevDigest == "" {
		t.Fatalf("announce must carry a digest, not the set: %d revocations, digest %q", len(ann.Revocations), ann.RevDigest)
	}
	go a.m.announceLoop()
	go b.m.announceLoop()
	a.m.ensureDial(b.srv.URL)
	waitFor(t, "b to learn the revocation via digest mismatch", 8*time.Second, func() bool {
		return len(b.m.state.revocationSnapshot()) == 1
	})
	if a.m.state.revocationDigest() != b.m.state.revocationDigest() {
		t.Fatal("digests must match after sync")
	}
	// Once equal, further announces must not trigger another pull.
	c := a.m.conns.get(b.m.id.NodeID)
	before := c.framesRecv.Load()
	time.Sleep(2500 * time.Millisecond) // two announce rounds at 1s
	if sent := c.framesRecv.Load() - before; sent > 12 {
		t.Fatalf("steady state is chatty: %d frames in 2.5s", sent)
	}
}
