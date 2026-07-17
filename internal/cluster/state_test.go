package cluster

import (
	"encoding/json"
	"testing"
)

// ---- credential extraction ----

func TestIsCredField(t *testing.T) {
	creds := []string{"refresh_token", "RefreshToken", "access_token", "cookie", "password", "client_secret", "api_key", "authorization", "session_id"}
	for _, c := range creds {
		if !isCredField(c) {
			t.Errorf("%q should be detected as a credential field", c)
		}
	}
	nonCreds := []string{"root_folder_id", "root_folder_path", "order_by", "region", "endpoint", "chunk_size", "device_id"}
	for _, n := range nonCreds {
		if isCredField(n) {
			t.Errorf("%q should NOT be detected as a credential field", n)
		}
	}
}

func TestExtractAndApplyCreds(t *testing.T) {
	addition := `{"root_folder_id":"123","refresh_token":"OLD","access_token":"AOLD","order_by":"name"}`
	creds := extractCreds(addition)
	if len(creds) != 2 {
		t.Fatalf("expected 2 credential fields, got %d (%v)", len(creds), creds)
	}
	if _, ok := creds["root_folder_id"]; ok {
		t.Fatal("structural field leaked into credential set")
	}

	// Apply new creds onto a peer Addition that has a DIFFERENT root folder.
	peer := `{"root_folder_id":"999","refresh_token":"OLDER","access_token":"AOLDER","order_by":"size"}`
	newCreds := map[string]json.RawMessage{
		"refresh_token": json.RawMessage(`"NEW"`),
		"access_token":  json.RawMessage(`"ANEW"`),
	}
	out, changed := applyCreds(peer, newCreds)
	if !changed {
		t.Fatal("applyCreds should report a change")
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	if m["refresh_token"] != "NEW" || m["access_token"] != "ANEW" {
		t.Fatalf("credentials not overlaid: %v", m)
	}
	// node-local fields preserved.
	if m["root_folder_id"] != "999" || m["order_by"] != "size" {
		t.Fatalf("node-local fields must be preserved: %v", m)
	}

	// Re-applying identical creds is a no-op (idempotent — no churn).
	if _, changed := applyCreds(out, newCreds); changed {
		t.Fatal("re-applying identical creds must not report a change")
	}
}

func TestCredHashIdempotent(t *testing.T) {
	a := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"x"`), "access_token": json.RawMessage(`"y"`)}
	b := map[string]json.RawMessage{"access_token": json.RawMessage(`"y"`), "refresh_token": json.RawMessage(`"x"`)}
	if credHash(a) != credHash(b) {
		t.Fatal("credHash must be order-independent")
	}
	if credHash(map[string]json.RawMessage{}) != "" {
		t.Fatal("empty creds must hash to empty string")
	}
	c := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"z"`)}
	if credHash(a) == credHash(c) {
		t.Fatal("different creds must hash differently")
	}
}

// ---- groups doc LWW ----

func TestGroupDocSignAndMerge(t *testing.T) {
	id1, _ := newIdentity()
	id2, _ := newIdentity()
	s := newStore()

	g := []group{{ID: "g1", Name: "115", Members: []member{{NodeID: id1.NodeID, MountPath: "/115"}}}}
	d1 := s.setGroups(id1, g, 100)
	if !d1.verify() {
		t.Fatal("authored groups doc must verify")
	}

	// A peer's newer doc dominates.
	d2 := groupDoc{
		Groups:    append(g, group{ID: "g2", Name: "ali", Members: []member{{NodeID: id2.NodeID, MountPath: "/ali"}}}),
		Version:   d1.Version + 5,
		Origin:    id2.NodeID,
		OriginPub: id2.Pub,
		UpdatedAt: 200,
	}
	d2.Sig = id2.sign(d2.signingBytes())
	if !s.mergeGroups(&d2) {
		t.Fatal("newer groups doc should be adopted")
	}
	if len(s.groupList()) != 2 {
		t.Fatalf("expected 2 groups after merge, got %d", len(s.groupList()))
	}
	// An older doc is ignored.
	if s.mergeGroups(&d1) {
		t.Fatal("older groups doc must be ignored")
	}
	// Tampered doc rejected.
	d2.Groups[0].Name = "tampered"
	if d2.verify() {
		t.Fatal("tampered groups doc must fail verification")
	}
}

func TestGroupDocRejectsNonCanonicalVersionZero(t *testing.T) {
	if !(&groupDoc{}).verify() {
		t.Fatal("the empty bootstrap document must remain valid")
	}
	id, _ := newIdentity()
	forged := &groupDoc{
		Groups:    []group{{ID: "g1", Members: []member{{NodeID: id.NodeID, MountPath: "/115"}}}},
		Origin:    id.NodeID,
		OriginPub: id.Pub,
		UpdatedAt: 1,
	}
	forged.Sig = id.sign(forged.signingBytes())
	if forged.verify() {
		t.Fatal("a non-empty version-zero group document must be rejected")
	}
}

func TestGroupsForMount(t *testing.T) {
	id, _ := newIdentity()
	s := newStore()
	s.setGroups(id, []group{
		{ID: "g1", Members: []member{{NodeID: "N1", MountPath: "/a"}, {NodeID: "N2", MountPath: "/a"}}},
		{ID: "g2", Members: []member{{NodeID: "N1", MountPath: "/b"}}},
	}, 1)
	if got := s.groupsForMount("N1", "/a"); len(got) != 1 || got[0].ID != "g1" {
		t.Fatalf("groupsForMount mismatch: %#v", got)
	}
	if got := s.groupsForMount("N3", "/a"); len(got) != 0 {
		t.Fatal("unknown node should match no groups")
	}
}

// ---- credential record LWW + idempotency ----

func TestCredRecordMergeLWW(t *testing.T) {
	id1, _ := newIdentity()
	id2, _ := newIdentity()
	s := newStore()
	payloadA := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"A"`)}
	r1, ok := s.localCredChange(id1, "g1", "115", "/115", payloadA, 1)
	if !ok || !r1.verify() {
		t.Fatal("first credential change should be recorded and verify")
	}
	// idempotent: same payload again -> no-op.
	if _, ok := s.localCredChange(id1, "g1", "115", "/115", payloadA, 2); ok {
		t.Fatal("identical credential must be a no-op")
	}

	// peer record with higher version dominates.
	payloadB := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"B"`)}
	r2 := &credRecord{GroupID: "g1", OriginDriver: "115", Fields: []string{"refresh_token"}, Payload: payloadB, Version: r1.Version + 10, Origin: id2.NodeID, OriginPub: id2.Pub, CredHash: credHash(payloadB)}
	r2.Sig = id2.sign(r2.signingBytes())
	if !r2.verify() {
		t.Fatal("peer record should verify")
	}
	if !s.mergeCred(r2) {
		t.Fatal("newer peer credential should win")
	}
	cur, _ := s.getCred("g1")
	if cur.CredHash != credHash(payloadB) {
		t.Fatal("store should hold the newer credential")
	}
	// older loses.
	if s.mergeCred(r1) {
		t.Fatal("older credential must be ignored")
	}
	// idempotent merge of identical hash.
	if s.mergeCred(r2) {
		t.Fatal("merging identical credential must be a no-op")
	}
}

// A group is a peer set, not a primary/replica pair. A newer credential from
// one healthy member must never erase a different member's candidate: each
// origin owns its current candidate and nodes choose what to activate locally.
func TestCredRecordMergePreservesCandidatesFromIndependentOrigins(t *testing.T) {
	id1, _ := newIdentity()
	id2, _ := newIdentity()
	s := newStore()

	payloadA := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"A"`)}
	if _, ok := s.localCredChange(id1, "g1", "115", "/115-a", payloadA, 1); !ok {
		t.Fatal("local candidate should be recorded")
	}

	payloadB := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"B"`)}
	peer := &credRecord{
		GroupID:      "g1",
		OriginDriver: "115",
		Fields:       []string{"refresh_token"},
		Payload:      payloadB,
		Version:      99, // Must not make this origin a group-wide primary.
		Origin:       id2.NodeID,
		OriginPub:    id2.Pub,
		OriginMount:  "/115-b",
		CredHash:     credHash(payloadB),
	}
	peer.Sig = id2.sign(peer.signingBytes())
	if !s.mergeCred(peer) {
		t.Fatal("peer candidate should merge")
	}

	got := s.credSnapshot()
	if len(got) != 2 {
		t.Fatalf("candidate count = %d, want 2 independent origins; snapshot=%#v", len(got), got)
	}
	seen := map[string]string{}
	for _, candidate := range got {
		seen[candidate.Origin] = candidate.CredHash
	}
	if seen[id1.NodeID] != credHash(payloadA) || seen[id2.NodeID] != credHash(payloadB) {
		t.Fatalf("candidate origins were not preserved: %#v", seen)
	}
}

func TestCandidateHealthLeaseExpires(t *testing.T) {
	s := newStore()
	const groupID = "g1"
	const hash = "candidate-hash"
	const provenAt int64 = 1_000

	s.markCandidateHealthy(groupID, hash, provenAt)
	if !s.candidateHealthy(groupID, hash, provenAt+candidateLeaseSec) {
		t.Fatal("candidate must remain offerable through its health lease")
	}
	if s.candidateHealthy(groupID, hash, provenAt+candidateLeaseSec+1) {
		t.Fatal("idle candidate must stop being offerable after its health lease")
	}

	s.markCandidateHealthy(groupID, hash, provenAt)
	s.forgetCandidateHealth(groupID, hash)
	if s.candidateHealthy(groupID, hash, provenAt) {
		t.Fatal("token-invalid must revoke the health lease immediately")
	}
}

func TestCredRecordVerifyRejectsForgery(t *testing.T) {
	id, _ := newIdentity()
	other, _ := newIdentity()
	payload := map[string]json.RawMessage{"token": json.RawMessage(`"x"`)}
	r := &credRecord{GroupID: "g", Payload: payload, CredHash: credHash(payload), Version: 1, Origin: id.NodeID, OriginPub: id.Pub}
	r.Sig = id.sign(r.signingBytes())
	// claim someone else's origin without their key.
	r.Origin = other.NodeID
	if r.verify() {
		t.Fatal("a record claiming a foreign origin must fail verification")
	}
}

// dropOwnCred must remove only our own-authored record so a dead/stale local
// token can never out-rank (by Lamport version) a peer's genuinely-valid one and
// block recovery. A peer-authored record must survive.
func TestDropOwnCred(t *testing.T) {
	id1, _ := newIdentity() // self
	id2, _ := newIdentity() // peer
	s := newStore()

	// nothing recorded yet -> nothing to drop.
	if s.dropOwnCred("g1", id1.NodeID) {
		t.Fatal("dropping a non-existent record must return false")
	}

	// our own credential -> dropped, store no longer holds it.
	payloadA := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"A"`)}
	if _, ok := s.localCredChange(id1, "g1", "115", "/115", payloadA, 1); !ok {
		t.Fatal("own credential should be recorded")
	}
	if !s.dropOwnCred("g1", id1.NodeID) {
		t.Fatal("own credential should be dropped")
	}
	if _, ok := s.getCred("g1"); ok {
		t.Fatal("store must not retain a dropped own credential")
	}

	// a peer's credential must NOT be dropped by us.
	payloadB := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"B"`)}
	r := &credRecord{GroupID: "g2", OriginDriver: "115", Fields: []string{"refresh_token"}, Payload: payloadB, Version: 5, Origin: id2.NodeID, OriginPub: id2.Pub, CredHash: credHash(payloadB)}
	r.Sig = id2.sign(r.signingBytes())
	if !s.mergeCred(r) {
		t.Fatal("peer credential should merge")
	}
	if s.dropOwnCred("g2", id1.NodeID) {
		t.Fatal("a peer-authored credential must never be dropped as own")
	}
	if _, ok := s.getCred("g2"); !ok {
		t.Fatal("peer credential must survive dropOwnCred")
	}
}

// canOfferCred enforces "only share a token proven valid" for every candidate.
// Receiving a signed peer record is not proof that this node can use it, so it
// must not turn this node into a blind relay for an unvalidated credential.
func TestCanOfferCred(t *testing.T) {
	id1, _ := newIdentity() // self
	id2, _ := newIdentity() // peer
	m := &Manager{id: id1, state: newStore()}

	if m.canOfferCred(nil) {
		t.Fatal("a nil record is never offerable")
	}

	// A peer-authored record is not offerable without a matching local healthy
	// mount. This manager has neither a group nor a live validated candidate.
	peer := &credRecord{GroupID: "g1", Origin: id2.NodeID}
	if m.canOfferCred(peer) {
		t.Fatal("a peer-authored record must NOT be relayed before local validation")
	}

	// our own record, but no group/healthy mount -> not offerable.
	own := &credRecord{GroupID: "g1", Origin: id1.NodeID}
	if m.canOfferCred(own) {
		t.Fatal("our own record must NOT be offered when the token is not proven healthy")
	}
}

func TestAbsorbCredsRejectsUnauthorizedOriginMount(t *testing.T) {
	admin, _ := newIdentity()
	memberID, _ := newIdentity()
	localID, _ := newIdentity()
	s := newStore()
	s.setGroups(admin, []group{{
		ID:      "g1",
		Members: []member{{NodeID: memberID.NodeID, MountPath: "/115"}},
	}}, 1)
	m := &Manager{id: localID, state: s, cfgStore: &configStore{cfg: Config{ApplyRemote: false}}}

	payload := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"candidate"`)}
	bad := &credRecord{
		GroupID:      "g1",
		OriginDriver: "115 Open",
		Fields:       []string{"refresh_token"},
		Payload:      payload,
		CredHash:     credHash(payload),
		Version:      2,
		Origin:       memberID.NodeID,
		OriginPub:    memberID.Pub,
		OriginMount:  "/not-a-group-member",
	}
	bad.Sig = memberID.sign(bad.signingBytes())
	if got := m.absorbCreds([]*credRecord{bad}); len(got) != 0 {
		t.Fatalf("unauthorized origin mount was accepted: %#v", got)
	}
	if _, ok := s.getCredForOrigin("g1", memberID.NodeID); ok {
		t.Fatal("unauthorized credential must not enter replicated state")
	}

	good := *bad
	good.OriginMount = "/115"
	good.Sig = memberID.sign(good.signingBytes())
	if got := m.absorbCreds([]*credRecord{&good}); len(got) != 1 {
		t.Fatalf("authorized group member credential rejected: %#v", got)
	}
}

// ---- inventory ----

func TestInventoryMergeAndDialTargets(t *testing.T) {
	s := newStore()
	n1 := &nodeInfo{NodeID: "N1", Addr: "https://n1", Version: 1}
	s.mergeInventory(n1, 1000)
	// newer version replaces.
	s.mergeInventory(&nodeInfo{NodeID: "N1", Addr: "https://n1b", Version: 2}, 1001)
	// older ignored (but liveness refreshed).
	s.mergeInventory(&nodeInfo{NodeID: "N1", Addr: "https://old", Version: 1}, 1002)
	list := s.inventoryList()
	if len(list) != 1 || list[0].Addr != "https://n1b" {
		t.Fatalf("inventory LWW failed: %#v", list)
	}
	s.mergeInventory(&nodeInfo{NodeID: "SELF", Addr: "https://self", Version: 1}, 1003)
	addrs := s.dialableAddrs("SELF")
	if len(addrs) != 1 || addrs[0] != "https://n1b" {
		t.Fatalf("dialableAddrs should exclude self: %#v", addrs)
	}
}
