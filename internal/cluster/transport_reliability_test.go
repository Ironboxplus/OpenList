package cluster

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"
)

func testCred(t *testing.T, id *identity, group, mount string, ver uint64, tok string, updatedAt int64) *credRecord {
	t.Helper()
	payload := map[string]json.RawMessage{"access_token": json.RawMessage(fmt.Sprintf("%q", tok)), "refresh_token": json.RawMessage(fmt.Sprintf("%q", tok+"-r"))}
	r := &credRecord{
		GroupID: group, OriginDriver: "115 Open", Fields: credFieldNames(payload), Payload: payload,
		CredHash: credHash(payload), Version: ver, Origin: id.NodeID, OriginPub: id.Pub, OriginMount: mount, UpdatedAt: updatedAt,
	}
	r.Sig = id.sign(r.signingBytes())
	r.MountSig = id.sign(r.mountSigningBytes())
	return r
}

// The old FIFO closed the whole link when 128 frames were queued. The outbox
// keeps one pending value per key, so a burst of any size stays bounded.
func TestOutboxCoalescesBurstWithoutGrowing(t *testing.T) {
	id, _ := newIdentity()
	o := newOutbox()
	recs := make([]*credRecord, 2000)
	for i := range recs {
		recs[i] = testCred(t, id, "g1", "/m", uint64(i+1), fmt.Sprintf("tok%d", i), int64(i))
	}
	for n := 0; n < 25; n++ {
		for i := 1; i <= 2000; i++ {
			o.put(&syncMessage{Type: "push", Creds: []*credRecord{recs[i-1]}})
			o.put(&syncMessage{Type: "announce", Node: &nodeInfo{NodeID: "n", Version: uint64(i)}})
			o.put(&syncMessage{Type: "pull", Wants: []string{"g1"}})
		}
	}
	if d := o.depth(); d > 4 {
		t.Fatalf("queue depth after a 150k-message burst = %d, want it coalesced to a handful", d)
	}
	pri := o.takePriority()
	if pri == nil || len(pri.Creds) != 1 || pri.Creds[0].Version != 2000 {
		t.Fatalf("priority flush must carry only the latest credential version, got %#v", pri)
	}
	if o.takePriority() != nil {
		t.Fatal("priority queue should be empty after flush")
	}
	bulk := o.takeBulk()
	if bulk == nil || bulk.Type != "announce" || bulk.Node.Version != 2000 {
		t.Fatalf("latest announce expected, got %#v", bulk)
	}
	if pull := o.takeBulk(); pull == nil || pull.Type != "pull" || len(pull.Wants) != 1 {
		t.Fatalf("one coalesced pull expected, got %#v", pull)
	}
}

func TestOutboxPrioritisesCredentialsAndRevocationsOverBulk(t *testing.T) {
	id, _ := newIdentity()
	o := newOutbox()
	o.put(&syncMessage{Type: "announce", Node: &nodeInfo{NodeID: "n", Version: 1}})
	o.put(&syncMessage{Type: "reply", Groups: &groupDoc{Version: 3}})
	rev := &credRevocation{GroupID: "g1", CredHash: "h", Version: 5}
	o.put(&syncMessage{Type: "push", Creds: []*credRecord{testCred(t, id, "g1", "/m", 2, "a", 1)}, Revocations: []*credRevocation{rev}})
	first := o.takePriority()
	if first == nil || len(first.Creds) != 1 || len(first.Revocations) != 1 {
		t.Fatalf("credential+revocation must flush first, got %#v", first)
	}
	if o.takePriority() != nil {
		t.Fatal("no more priority state expected")
	}
	if b := o.takeBulk(); b == nil || b.Type != "announce" {
		t.Fatalf("bulk announce expected next, got %#v", b)
	}
	if b := o.takeBulk(); b == nil || b.Groups == nil || b.Groups.Version != 3 {
		t.Fatalf("bulk group doc expected, got %#v", b)
	}
}

func TestOutboxKeepsDominatingValuePerKey(t *testing.T) {
	id, _ := newIdentity()
	o := newOutbox()
	newer := testCred(t, id, "g1", "/m", 9, "new", 9)
	older := testCred(t, id, "g1", "/m", 4, "old", 4)
	o.put(&syncMessage{Type: "push", Creds: []*credRecord{newer}})
	o.put(&syncMessage{Type: "push", Creds: []*credRecord{older}}) // arrives later but is older
	got := o.takePriority()
	if got == nil || len(got.Creds) != 1 || got.Creds[0].Version != 9 {
		t.Fatalf("an older record must never replace a newer pending one: %#v", got)
	}
	// Distinct source mounts are distinct keys.
	o.put(&syncMessage{Type: "push", Creds: []*credRecord{testCred(t, id, "g1", "/a", 1, "a", 1), testCred(t, id, "g1", "/b", 1, "b", 1)}})
	if got := o.takePriority(); got == nil || len(got.Creds) != 2 {
		t.Fatalf("per-(group,origin,mount) candidates must all survive: %#v", got)
	}
}

func TestOutboxDirectedMessagesAreBounded(t *testing.T) {
	o := newOutbox()
	for i := 0; i < 1000; i++ {
		o.put(&syncMessage{Type: msgRefreshRequest, To: "peer", Req: &refreshRequest{GroupID: "g", HaveVersion: uint64(i)}})
	}
	if o.depth() > maxDirected {
		t.Fatalf("directed queue grew to %d, cap is %d", o.depth(), maxDirected)
	}
	last := uint64(0)
	for m := o.takePriority(); m != nil; m = o.takePriority() {
		last = m.Req.HaveVersion
	}
	if last != 999 {
		t.Fatalf("newest directed message must survive the cap, last=%d", last)
	}
}

func TestOutboxAbsorbMovesPendingStateToSurvivor(t *testing.T) {
	id, _ := newIdentity()
	loser, winner := newOutbox(), newOutbox()
	loser.put(&syncMessage{Type: "push", Creds: []*credRecord{testCred(t, id, "g1", "/m", 3, "x", 3)}})
	loser.put(&syncMessage{Type: msgAck, To: "p", Acks: []credDigest{{GroupID: "g1"}}})
	winner.absorb(loser)
	if loser.depth() != 0 {
		t.Fatalf("loser still holds %d items", loser.depth())
	}
	var creds, acks int
	for m := winner.takePriority(); m != nil; m = winner.takePriority() {
		creds += len(m.Creds)
		acks += len(m.Acks)
	}
	if creds != 1 || acks != 1 {
		t.Fatalf("survivor lost state: creds=%d acks=%d", creds, acks)
	}
}

// A queue backlog must never close a link; only write errors do.
func TestPeerConnEnqueueNeverClosesTheLink(t *testing.T) {
	c := &peerConn{out: newOutbox(), closed: make(chan struct{})}
	id, _ := newIdentity()
	pre := make([]*credRecord, 50)
	for i := range pre {
		pre[i] = testCred(t, id, "g1", fmt.Sprintf("/m%d", i), 1, "t", 1)
	}
	for i := 0; i < 100000; i++ {
		c.enqueue(&syncMessage{Type: "push", Creds: []*credRecord{pre[i%50]}})
	}
	if c.isClosed() {
		t.Fatal("link closed because of backlog")
	}
	if d := c.queueDepth(); d > 50 {
		t.Fatalf("depth %d exceeds the number of distinct keys", d)
	}
}

// Both ends of a duplicated pair must keep the same physical link.
func TestKeepNewLinkBothEndsAgree(t *testing.T) {
	ids := []string{"AAA", "BBB"}
	// Physical links: X dialed by A, Y dialed by B. Each end sees them in either order.
	for _, firstSeen := range []string{"X", "Y"} {
		survivors := map[string]string{}
		for _, self := range ids {
			peer := ids[0]
			if self == ids[0] {
				peer = ids[1]
			}
			outbound := func(link string) bool { // did *this end* dial the link?
				if link == "X" {
					return self == "AAA"
				}
				return self == "BBB"
			}
			reg := newConnRegistry()
			links := map[string]*peerConn{
				"X": {closed: make(chan struct{}), outbound: outbound("X")},
				"Y": {closed: make(chan struct{}), outbound: outbound("Y")},
			}
			order := []string{firstSeen, map[string]string{"X": "Y", "Y": "X"}[firstSeen]}
			var closed []string
			for _, name := range order {
				c := links[name]
				reg.add(c)
				if loser := reg.bind(c, peer, self); loser != nil {
					for n, l := range links {
						if l == loser {
							closed = append(closed, n)
						}
					}
					loser.close("test")
				}
			}
			if len(closed) != 1 {
				t.Fatalf("self=%s: want exactly one link closed, closed=%v", self, closed)
			}
			survivors[self] = closed[0]
		}
		if survivors["AAA"] != survivors["BBB"] {
			t.Fatalf("ends disagree on the loser (first seen %s): %v", firstSeen, survivors)
		}
		// The preferred dialer is the smaller node id: link X (dialed by AAA) survives.
		if survivors["AAA"] != "Y" {
			t.Fatalf("link dialed by the smaller id must survive, but %s was closed", survivors["AAA"])
		}
	}
}

func TestDialBackoffGrowsCapsAndJitters(t *testing.T) {
	d := time.Duration(0)
	var seq []time.Duration
	for i := 0; i < 10; i++ {
		d = nextBackoff(d)
		seq = append(seq, d)
	}
	if seq[0] != dialBackoffMin || seq[1] != 2*dialBackoffMin {
		t.Fatalf("backoff must start at the minimum and double: %v", seq)
	}
	if seq[len(seq)-1] != dialBackoffMax {
		t.Fatalf("backoff must cap at %s: %v", dialBackoffMax, seq)
	}
	distinct := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		j := jitterDuration(40 * time.Second)
		if j < 30*time.Second || j > 50*time.Second {
			t.Fatalf("jitter %s outside +-25%% of 40s", j)
		}
		distinct[j] = true
	}
	if len(distinct) < 20 {
		t.Fatal("jitter is not random enough to de-synchronise redials")
	}
	if jitterDuration(0) != 0 {
		t.Fatal("zero backoff must stay zero (stable link redials at once)")
	}
}

// ---- revocations ----

func TestRevocationDigestIsOrderIndependentAndSensitive(t *testing.T) {
	a := &credRevocation{GroupID: "g", CredHash: "h1", Version: 3, Origin: "o"}
	b := &credRevocation{GroupID: "g", CredHash: "h2", Version: 4, Origin: "o"}
	if revocationDigest([]*credRevocation{a, b}) != revocationDigest([]*credRevocation{b, a}) {
		t.Fatal("digest must not depend on order")
	}
	if revocationDigest([]*credRevocation{a}) == revocationDigest([]*credRevocation{a, b}) {
		t.Fatal("digest must change when an entry is added")
	}
	c := *a
	c.Version = 9
	if revocationDigest([]*credRevocation{a}) == revocationDigest([]*credRevocation{&c}) {
		t.Fatal("digest must change when an entry is replaced by a dominating one")
	}
	if revocationDigest(nil) != "" {
		t.Fatal("empty set digests to the empty string")
	}
}

func newRevState(t *testing.T) (*store, *identity, *identity, *int64) {
	t.Helper()
	admin, _ := newIdentity()
	src, _ := newIdentity()
	s := newStore()
	clock := new(int64)
	*clock = 1_000_000
	s.clock = func() int64 { return *clock }
	s.revRetentionSec = 1000
	s.setGroups(admin, []group{{ID: "g1", Members: []member{{NodeID: src.NodeID, MountPath: "/m"}, {NodeID: src.NodeID, MountPath: "/n"}}}}, 1)
	return s, admin, src, clock
}

// The ordering argument: a pruned tombstone must not let its dead pair return.
func TestPrunedRevocationCannotResurrectPair(t *testing.T) {
	s, _, src, clock := newRevState(t)
	dead := testCred(t, src, "g1", "/m", 10, "dead", 1_000_000-500) // authored before it was rejected
	if !s.mergeCred(dead) {
		t.Fatal("setup: dead pair should merge before it is revoked")
	}
	rev, ok := s.revokePair(src, "g1", "/m", dead.CredHash, 1_000_000-400)
	if !ok {
		t.Fatal("setup: revoke")
	}
	// A successor exists, authored after the revocation.
	good := testCred(t, src, "g1", "/m", 11, "good", 1_000_000-300)
	if !s.mergeCred(good) {
		t.Fatal("setup: successor must merge")
	}

	// Inside the retention window nothing is pruned.
	*clock = 1_000_000
	if n := s.pruneRevocations(); n != 0 {
		t.Fatalf("pruned %d inside the retention window", n)
	}
	// Past retention it is pruned...
	*clock = 1_000_000 + 2000
	if n := s.pruneRevocations(); n != 1 {
		t.Fatalf("expected the superseded revocation to be pruned, got %d", n)
	}
	if got := len(s.revocationSnapshot()); got != 0 {
		t.Fatalf("revocation still present: %d", got)
	}
	// ...yet the dead pair, replayed by an offline node that still holds it, is refused.
	if s.mergeCred(dead) {
		t.Fatal("pruned revocation let the dead pair resurrect")
	}
	// Even attributed to a different source mount of the same node.
	other := testCred(t, src, "g1", "/n", 12, "dead", 1_000_000-500)
	if s.mergeCred(other) {
		t.Fatal("dead pair re-authored under another mount resurrected")
	}
	// A later re-delivery of the old tombstone must not re-grow the set.
	if s.mergeRevocation(rev) {
		t.Fatal("an expired, superseded revocation must not be re-adopted")
	}
	// The current pair is untouched and a genuinely newer rotation is accepted.
	if _, ok := s.getCredForSource("g1", src.NodeID, "/m"); !ok {
		t.Fatal("live credential was lost")
	}
	next := testCred(t, src, "g1", "/m", 13, "next", 1_000_000+900)
	if !s.mergeCred(next) {
		t.Fatal("a newer credential must still be accepted after pruning")
	}
}

func TestRevocationNotPrunedWithoutSuccessor(t *testing.T) {
	s, _, src, clock := newRevState(t)
	dead := testCred(t, src, "g1", "/m", 10, "dead", 1_000_000-500)
	s.mergeCred(dead)
	s.revokePair(src, "g1", "/m", dead.CredHash, 1_000_000-400)
	*clock = 1_000_000 + 10_000
	if n := s.pruneRevocations(); n != 0 {
		t.Fatalf("a revocation with no newer live credential must be kept, pruned=%d", n)
	}
	if s.mergeCred(dead) {
		t.Fatal("dead pair accepted")
	}
}

// Lamport order alone is not enough: a reporter that never saw the dead record
// can mint a revocation whose version is lower than the record's.
func TestPruneHoldsEvenWhenRevocationVersionIsBelowTheRecordVersion(t *testing.T) {
	s, _, src, clock := newRevState(t)
	reporter, _ := newIdentity()
	s.setGroups(reporter, []group{{ID: "g1", Members: []member{{NodeID: src.NodeID, MountPath: "/m"}, {NodeID: reporter.NodeID, MountPath: "/r"}}}}, 2)
	// Dead record with a HIGH Lamport version, never observed by the reporter.
	dead := testCred(t, src, "g1", "/m", 9000, "dead", 1_000_000-500)
	// The reporter's revocation has a LOW Lamport version but a later wall clock.
	rev := &credRevocation{GroupID: "g1", Origin: reporter.NodeID, OriginPub: reporter.Pub, OriginMount: "/r",
		CredHash: dead.CredHash, Version: 5, UpdatedAt: 1_000_000 - 400}
	rev.Sig = reporter.sign(rev.signingBytes())
	if !s.mergeRevocation(rev) {
		t.Fatal("setup: revocation")
	}
	good := testCred(t, src, "g1", "/m", 9001, "good", 1_000_000-300)
	s.mergeCred(good)
	*clock = 1_000_000 + 1000
	if n := s.pruneRevocations(); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	if rev.Version >= dead.Version {
		t.Fatal("test premise: revocation version must be below the record's")
	}
	if s.mergeCred(dead) {
		t.Fatal("dead pair with a higher Lamport version resurrected after pruning")
	}
}

func TestRevocationsSurviveRestartKeyedByPair(t *testing.T) {
	s, _, src, _ := newRevState(t)
	dead := testCred(t, src, "g1", "/m", 10, "dead", 100)
	s.mergeCred(dead)
	s.revokePair(src, "g1", "/m", dead.CredHash, 200)
	ps := s.export()
	s2 := newStore()
	s2.load(ps)
	if s2.mergeCred(dead) {
		t.Fatal("after a restart the revoked pair was accepted again (revocations must reload keyed by pair hash)")
	}
	if len(s2.revocationSnapshot()) != 1 {
		t.Fatal("revocation lost across restart")
	}
}

// ---- leases ----

func TestLeaseDominance(t *testing.T) {
	a, _ := newIdentity()
	b, _ := newIdentity()
	s := newStore()
	hi, lo := a, b
	if lo.NodeID > hi.NodeID {
		hi, lo = lo, hi
	}
	ttl := int64(60)
	base := time.Now()
	l1 := s.issueLease(lo, "g", 1, 10, ttl, nil, base)
	l2 := s.issueLease(hi, "g", 1, 10, ttl, nil, base)
	if !l2.dominates(l1) || l1.dominates(l2) {
		t.Fatal("same epoch and seq: the larger node id wins the tie")
	}
	l3 := s.issueLease(lo, "g", 1, 11, ttl, nil, base)
	if !l3.dominates(l2) {
		t.Fatal("higher seq must dominate within an epoch")
	}
	l4 := s.issueLease(lo, "g", 2, 1, ttl, nil, base)
	if !l4.dominates(l3) {
		t.Fatal("a higher epoch dominates any seq")
	}
	for _, l := range []*refresherLease{l1, l2, l3, l4} {
		if !l.verify() {
			t.Fatal("issued lease must verify")
		}
	}
	forged := *l4
	forged.Epoch = 99
	if forged.verify() {
		t.Fatal("tampered epoch must fail verification")
	}
	forged2 := *l4
	forged2.Last = &refreshSummary{Outcome: "ok"}
	if forged2.verify() {
		t.Fatal("tampered refresh summary must fail verification")
	}
}

func TestLeaseExpiryUsesReceiverClockNotDocumentTimestamps(t *testing.T) {
	holder, _ := newIdentity()
	sender := newStore()
	l := sender.issueLease(holder, "g", 1, 1, 30, nil, time.Now())
	l.IssuedAt = time.Now().Add(-48 * time.Hour).Unix() // wildly skewed sender clock is irrelevant
	rcv := newStore()
	heard := time.Now()
	if !rcv.mergeLease(l, heard) {
		t.Fatal("merge")
	}
	e := rcv.leaseOf("g")
	if !e.valid(heard.Add(29 * time.Second)) {
		t.Fatal("lease must be valid within TTL of when it was heard")
	}
	if e.valid(heard.Add(31 * time.Second)) {
		t.Fatal("lease must lapse TTL after it was last heard")
	}
	// A replay of the same lease does not extend it.
	rcv.mergeLease(l, heard.Add(25*time.Second))
	if rcv.leaseOf("g").valid(heard.Add(31 * time.Second)) {
		t.Fatal("replaying an old lease must not extend validity")
	}
	// A renewal does.
	renew := sender.issueLease(holder, "g", 1, 2, 30, nil, time.Now())
	rcv.mergeLease(renew, heard.Add(25*time.Second))
	if !rcv.leaseOf("g").valid(heard.Add(50 * time.Second)) {
		t.Fatal("renewal must extend validity")
	}
	// A release (TTL 0) is immediately invalid.
	rel := sender.issueLease(holder, "g", 1, 3, 0, nil, time.Now())
	rcv.mergeLease(rel, heard.Add(26*time.Second))
	if rcv.leaseOf("g").valid(heard.Add(26 * time.Second)) {
		t.Fatal("released lease must be invalid at once")
	}
}

func TestLeaseOnlyAuthorizedForGroupMembers(t *testing.T) {
	member1, _ := newIdentity()
	outsider, _ := newIdentity()
	s := newStore()
	groups := []group{{ID: "g", Members: []member{{NodeID: member1.NodeID, MountPath: "/m"}}}}
	if !leaseAuthorizedByGroups(groups, s.issueLease(member1, "g", 1, 1, 60, nil, time.Now())) {
		t.Fatal("a member may hold the lease")
	}
	if leaseAuthorizedByGroups(groups, s.issueLease(outsider, "g", 1, 1, 60, nil, time.Now())) {
		t.Fatal("a non-member must not hold the lease")
	}
	if leaseAuthorizedByGroups(groups, s.issueLease(member1, "other", 1, 1, 60, nil, time.Now())) {
		t.Fatal("a lease for an unknown group must be rejected")
	}
}

func TestLeaseAndPolicyPersistAcrossRestart(t *testing.T) {
	holder, _ := newIdentity()
	admin, _ := newIdentity()
	s := newStore()
	s.setGroups(admin, []group{{ID: "g", Members: []member{{NodeID: holder.NodeID, MountPath: "/m"}}}}, 1)
	s.issueLease(holder, "g", 4, 77, 60, &refreshSummary{Time: 5, Outcome: "ok"}, time.Now())
	s.setPolicy(admin, "g", holder.NodeID, 3)
	s2 := newStore()
	s2.load(s.export())
	e := s2.leaseOf("g")
	if e == nil || e.lease.Epoch != 4 || e.lease.Last == nil || e.lease.Last.Outcome != "ok" {
		t.Fatalf("lease not restored: %#v", e)
	}
	if !e.valid(time.Now()) {
		t.Fatal("a restored lease is treated as just heard")
	}
	if p := s2.policyOf("g"); p == nil || p.Preferred != holder.NodeID {
		t.Fatalf("policy not restored: %#v", p)
	}
}

func TestPolicyLWWAndVerify(t *testing.T) {
	a, _ := newIdentity()
	s := newStore()
	p1 := s.setPolicy(a, "g", "n1", 1)
	p2 := s.setPolicy(a, "g", "n2", 2)
	if !p2.dominates(p1) || !p1.verify() || !p2.verify() {
		t.Fatal("policy versions must order and verify")
	}
	other := newStore()
	other.mergePolicy(p2)
	if other.mergePolicy(p1) {
		t.Fatal("an older policy must not replace a newer one")
	}
	bad := *p2
	bad.Preferred = "evil"
	if bad.verify() {
		t.Fatal("tampered preferred node must fail verification")
	}
}

func TestTakeoverOrderPreferredThenWorkingThenPublicThenID(t *testing.T) {
	cands := []refreshCandidate{
		{NodeID: "D", Public: false, Working: true},
		{NodeID: "C", Public: true, Working: true},
		{NodeID: "B", Public: true, Working: false},
		{NodeID: "A", Public: false, Working: true, Preferred: false},
		{NodeID: "Z", Public: false, Working: false, Preferred: true},
	}
	got := takeoverOrder(cands)
	want := []string{"Z", "C", "A", "D", "B"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	// Input order must not matter.
	sort.Slice(cands, func(i, j int) bool { return cands[i].NodeID > cands[j].NodeID })
	if fmt.Sprint(takeoverOrder(cands)) != fmt.Sprint(want) {
		t.Fatal("takeover order must be deterministic")
	}
}
