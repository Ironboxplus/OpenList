package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	open115 "github.com/OpenListTeam/OpenList/v4/drivers/115_open"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/local"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
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

// A complete credential pair has one group-wide identity. Seeing the same pair
// on another member must not turn it into a newer, independently revocable
// candidate, and a provider-auth rejection must suppress every replay of it.
func TestPairIdentityPreventsReauthoringAndBlocksReplayAfterRejection(t *testing.T) {
	a, _ := newIdentity()
	b, _ := newIdentity()
	s := newStore()
	pair := map[string]json.RawMessage{
		"access_token":  json.RawMessage(`"access-A"`),
		"refresh_token": json.RawMessage(`"refresh-A"`),
	}

	first, ok := s.localCredChange(a, "g1", "115 Open", "/storage/115", pair, 1)
	if !ok {
		t.Fatal("first locally proven pair should be recorded")
	}
	if _, ok := s.localCredChange(b, "g1", "115 Open", "/storage/115", pair, 2); ok {
		t.Fatal("an imported pair must retain its original provenance, not be re-authored by another node")
	}
	if got := len(s.credsForGroup("g1")); got != 1 {
		t.Fatalf("same pair must have one catalog entry, got %d", got)
	}

	rev, ok := s.revokePair(a, "g1", "/storage/115", first.CredHash, 3)
	if !ok {
		t.Fatal("provider-auth rejection must create a pair-level tombstone")
	}
	if got := len(s.credsForGroup("g1")); got != 0 {
		t.Fatalf("pair tombstone must remove every copy, got %d", got)
	}
	if s.mergeCred(first) {
		t.Fatal("a rejected pair must not be replayed by its original signer")
	}
	if _, ok := s.localCredChange(b, "g1", "115 Open", "/storage/115", pair, 4); ok {
		t.Fatal("a rejected pair must not re-enter under another node identity")
	}
	if !s.mergeRevocation(rev) {
		// A locally created tombstone is already present; repeated delivery must
		// be a harmless no-op rather than changing catalog state.
		if got := len(s.credsForGroup("g1")); got != 0 {
			t.Fatalf("replayed tombstone changed catalog to %d entries", got)
		}
	}
}

// A legacy peer may already have re-authored a pair before v2 reaches every
// node. Receiving that second signature must not recreate duplicate authority.
func TestMergeCredRejectsPeerReauthoringOfExistingPair(t *testing.T) {
	a, _ := newIdentity()
	b, _ := newIdentity()
	s := newStore()
	pair := map[string]json.RawMessage{
		"access_token":  json.RawMessage(`"access-A"`),
		"refresh_token": json.RawMessage(`"refresh-A"`),
	}
	first, ok := s.localCredChange(a, "g1", "115 Open", "/storage/115", pair, 1)
	if !ok {
		t.Fatal("first pair should be recorded")
	}
	reauthored := &credRecord{
		GroupID:      first.GroupID,
		OriginDriver: first.OriginDriver,
		Fields:       first.Fields,
		CredHash:     first.CredHash,
		Payload:      first.Payload,
		Version:      first.Version + 100,
		Origin:       b.NodeID,
		OriginPub:    b.Pub,
		OriginMount:  "/storage/115",
		UpdatedAt:    2,
	}
	reauthored.Sig = b.sign(reauthored.signingBytes())
	reauthored.MountSig = b.sign(reauthored.mountSigningBytes())
	if s.mergeCred(reauthored) {
		t.Fatal("same pair from another origin must not be accepted as a newer candidate")
	}
	if got := len(s.credsForGroup("g1")); got != 1 {
		t.Fatalf("re-authored pair created %d catalog records, want 1", got)
	}
}

// Storage updates are not credential-validation events. In particular,
// UpdateStorage after receiving an imported pair must not re-sign and publish it
// from the receiving node. This uses the real 115 driver type and real manager
// state; no test driver or callback mock is involved.
func TestOnlyTokenValidMayPublishStoragePair(t *testing.T) {
	id, _ := newIdentity()
	s := newStore()
	s.setGroups(id, []group{{
		ID:      "g1",
		Members: []member{{NodeID: id.NodeID, MountPath: "/storage/115"}},
	}}, 1)
	m := &Manager{
		dir:      t.TempDir(),
		id:       id,
		state:    s,
		conns:    newConnRegistry(),
		cfgStore: &configStore{cfg: Config{Enabled: true, Key: "test-key", ApplyRemote: true}},
	}
	d := &open115.Open115{Storage: model.Storage{
		MountPath: "/storage/115",
		Driver:    "115 Open",
		Status:    op.WORK,
		Addition:  `{"access_token":"access-A","refresh_token":"refresh-A"}`,
	}}

	m.onStorageHook("update", d)
	if got := len(s.credsForGroup("g1")); got != 0 {
		t.Fatalf("ordinary update published %d credential record(s), want 0", got)
	}

	m.onStorageCredential("token-valid", op.StorageCredentialEvent{Storage: d, Addition: d.GetStorage().Addition})
	if got := len(s.credsForGroup("g1")); got != 1 {
		t.Fatalf("token-valid published %d credential record(s), want 1", got)
	}
}

// An authentication result belongs to the pair sent with that request, not to
// whatever happens to be mounted when its goroutine finally runs. This uses the
// real 115 driver type and exercises the manager's generation guard without
// fabricating a driver implementation.
func TestLateCredentialEventCannotRevokeReplacementPair(t *testing.T) {
	id, _ := newIdentity()
	s := newStore()
	s.setGroups(id, []group{{
		ID:      "g1",
		Members: []member{{NodeID: id.NodeID, MountPath: "/storage/115"}},
	}}, 1)
	newPair := `{"access_token":"new-access","refresh_token":"new-refresh"}`
	d := &open115.Open115{Storage: model.Storage{
		MountPath: "/storage/115",
		Driver:    "115 Open",
		Status:    op.WORK,
		Addition:  newPair,
	}}
	m := &Manager{
		dir:      t.TempDir(),
		id:       id,
		state:    s,
		conns:    newConnRegistry(),
		cfgStore: &configStore{cfg: Config{Enabled: true, Key: "test-key", ApplyRemote: true}},
	}
	oldPair := `{"access_token":"old-access","refresh_token":"old-refresh"}`
	m.onStorageCredential("token-invalid", op.StorageCredentialEvent{Storage: d, Addition: oldPair})
	if d.GetStorage().Status != op.WORK {
		t.Fatalf("late old-pair 401 changed replacement status to %q", d.GetStorage().Status)
	}
	if got := len(s.revocationSnapshot()); got != 0 {
		t.Fatalf("late old-pair 401 wrote %d revocations", got)
	}
}

// Candidate verification must be hermetic with respect to the live storage
// record. A temporary driver's successful Init can emit lifecycle callbacks, so
// its storage identity must never point at the persisted mount being recovered.
func TestProbeStorageIsEphemeralAndKeepsCandidateAddition(t *testing.T) {
	live := model.Storage{
		ID:        42,
		MountPath: "/storage/115",
		Driver:    "115 Open",
		Status:    "token invalid",
		Addition:  `{"access_token":"old","refresh_token":"old"}`,
	}
	candidate := `{"access_token":"new","refresh_token":"new"}`

	probe := newProbeStorage(live, candidate)
	if probe.ID != 0 {
		t.Fatalf("probe storage ID = %d, want 0 so callbacks cannot write the live row", probe.ID)
	}
	if probe.Status != op.WORK {
		t.Fatalf("probe storage status = %q, want %q to suppress token-valid lifecycle writes", probe.Status, op.WORK)
	}
	if probe.Addition != candidate {
		t.Fatalf("probe addition = %q, want candidate payload", probe.Addition)
	}
	if probe.MountPath != live.MountPath || probe.Driver != live.Driver {
		t.Fatalf("probe changed mount identity: %#v", probe)
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
	r.MountSig = id.sign(r.mountSigningBytes())
	// claim someone else's origin without their key.
	r.Origin = other.NodeID
	if r.verify() {
		t.Fatal("a record claiming a foreign origin must fail verification")
	}
}

func TestCredRecordVerifyBindsOriginMount(t *testing.T) {
	id, _ := newIdentity()
	payload := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"x"`)}
	r := &credRecord{
		GroupID:      "g",
		OriginDriver: "115 Open",
		Payload:      payload,
		CredHash:     credHash(payload),
		Version:      1,
		Origin:       id.NodeID,
		OriginPub:    id.Pub,
		OriginMount:  "/115-a",
	}
	r.Sig = id.sign(r.signingBytes())
	r.MountSig = id.sign(r.mountSigningBytes())
	if !r.verify() {
		t.Fatal("freshly signed record must verify")
	}
	r.OriginMount = "/115-b"
	if r.verify() {
		t.Fatal("changing origin mount must invalidate the credential signature")
	}
}

// legacyCredSigningBytes is the credential wire-signature format used before
// OriginMount was bound separately. Keeping this fixture lets an upgrade prove
// it can still read state.json written by the deployed 53bc1db protocol.
func legacyCredSigningBytes(r *credRecord) []byte {
	var b []byte
	b = append(b, r.GroupID...)
	b = append(b, 0)
	b = append(b, r.OriginDriver...)
	b = append(b, 0)
	b = append(b, r.CredHash...)
	b = append(b, 0)
	v := make([]byte, 8)
	for i := 0; i < 8; i++ {
		v[i] = byte(r.Version >> (8 * uint(i)))
	}
	b = append(b, v...)
	b = append(b, 0)
	b = append(b, r.Origin...)
	return b
}

func TestCredRecordVerifiesLegacySignature(t *testing.T) {
	id, _ := newIdentity()
	payload := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"legacy"`)}
	r := &credRecord{
		GroupID:      "g",
		OriginDriver: "115 Open",
		Payload:      payload,
		CredHash:     credHash(payload),
		Version:      7,
		Origin:       id.NodeID,
		OriginPub:    id.Pub,
		OriginMount:  "/115",
	}
	r.Sig = id.sign(legacyCredSigningBytes(r))
	if !r.verify() {
		t.Fatal("a 53bc1db credential must remain verifiable after upgrade")
	}
}

func TestLocalCredChangeMigratesLegacySourceWithoutTokenChange(t *testing.T) {
	id, _ := newIdentity()
	s := newStore()
	payload := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"legacy"`)}
	legacy := &credRecord{
		GroupID:      "g",
		OriginDriver: "115 Open",
		Payload:      payload,
		CredHash:     credHash(payload),
		Version:      1,
		Origin:       id.NodeID,
		OriginPub:    id.Pub,
		OriginMount:  "/115",
	}
	legacy.Sig = id.sign(legacyCredSigningBytes(legacy))
	if !s.mergeCred(legacy) {
		t.Fatal("legacy credential should be accepted into local state")
	}
	migrated, ok := s.localCredChange(id, "g", "115 Open", "/115", payload, 2)
	if !ok || migrated == nil || len(migrated.MountSig) == 0 {
		t.Fatal("a healthy local source must rewrite its legacy record with MountSig")
	}
	if migrated.signatureKind() != credentialSignatureMountBound {
		t.Fatal("migrated record must use the rolling-compatible mount-bound format")
	}
}

func TestSameOriginMultipleMountCandidatesArePreserved(t *testing.T) {
	id, _ := newIdentity()
	s := newStore()
	first := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"first"`)}
	second := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"second"`)}
	if _, ok := s.localCredChange(id, "g", "115 Open", "/115-a", first, 1); !ok {
		t.Fatal("first local mount candidate was not recorded")
	}
	if _, ok := s.localCredChange(id, "g", "115 Open", "/115-b", second, 2); !ok {
		t.Fatal("second local mount candidate was not recorded")
	}
	got := s.credsForGroup("g")
	if len(got) != 2 {
		t.Fatalf("candidate count = %d, want separate candidates for both mounts: %#v", len(got), got)
	}
	if !s.dropOwnCred("g", id.NodeID, "/115-a") {
		t.Fatal("invalidating one source mount must drop that source candidate")
	}
	remaining := s.credsForGroup("g")
	if len(remaining) != 1 || remaining[0].OriginMount != "/115-b" {
		t.Fatalf("invalidating /115-a removed the wrong candidate: %#v", remaining)
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
	if s.dropOwnCred("g1", id1.NodeID, "/115") {
		t.Fatal("dropping a non-existent record must return false")
	}

	// our own credential -> dropped, store no longer holds it.
	payloadA := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"A"`)}
	if _, ok := s.localCredChange(id1, "g1", "115", "/115", payloadA, 1); !ok {
		t.Fatal("own credential should be recorded")
	}
	if !s.dropOwnCred("g1", id1.NodeID, "/115") {
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
	if s.dropOwnCred("g2", id1.NodeID, "/115") {
		t.Fatal("a peer-authored credential must never be dropped as own")
	}
	if _, ok := s.getCred("g2"); !ok {
		t.Fatal("peer credential must survive dropOwnCred")
	}
}

// A source that has proved one of its credentials invalid must be able to
// prevent an old signed copy held by a relay from entering its state again.
// Without this, the source drops its own candidate, sends a pull, and a hub
// immediately returns the exact same stale candidate as a peer record.
func TestDroppedOwnCredentialRejectsStaleRelayReplay(t *testing.T) {
	source, _ := newIdentity()
	s := newStore()
	payload := map[string]json.RawMessage{
		"access_token":  json.RawMessage(`"dead-access"`),
		"refresh_token": json.RawMessage(`"dead-refresh"`),
	}
	record, ok := s.localCredChange(source, "g1", "115 Open", "/storage/115", payload, 1)
	if !ok {
		t.Fatal("source candidate was not recorded")
	}
	if rev, ok := s.revokeOwnCred(source, "g1", "/storage/115", 2); !ok || !rev.verify() {
		t.Fatal("source candidate was not revoked after invalidation")
	}

	if s.mergeCred(record) {
		t.Fatal("stale signed candidate from a relay was accepted after source invalidation")
	}
	if _, ok := s.getCredForSource("g1", source.NodeID, "/storage/115"); ok {
		t.Fatal("revoked source candidate re-entered state")
	}
}

func TestRecoveryCandidatesSkipCurrentAndQuarantinedCredentials(t *testing.T) {
	idA, _ := newIdentity()
	idB, _ := newIdentity()
	idC, _ := newIdentity()
	s := newStore()
	for _, tc := range []struct {
		id      *identity
		mount   string
		payload map[string]json.RawMessage
	}{
		{idA, "/a", map[string]json.RawMessage{"access_token": json.RawMessage(`"A"`)}},
		{idB, "/b", map[string]json.RawMessage{"access_token": json.RawMessage(`"B"`)}},
		{idC, "/c", map[string]json.RawMessage{"access_token": json.RawMessage(`"C"`)}},
	} {
		if _, ok := s.localCredChange(tc.id, "g1", "115 Open", tc.mount, tc.payload, 1); !ok {
			t.Fatalf("candidate %s was not recorded", tc.mount)
		}
	}
	failedHash := credHash(map[string]json.RawMessage{"access_token": json.RawMessage(`"A"`)})
	currentHash := credHash(map[string]json.RawMessage{"access_token": json.RawMessage(`"B"`)})
	s.markCandidateFailed("g1", "/local", failedHash, 2)

	candidates := s.recoveryCandidates("g1", "/local", currentHash)
	if len(candidates) != 1 || candidates[0].CredHash != credHash(map[string]json.RawMessage{"access_token": json.RawMessage(`"C"`)}) {
		t.Fatalf("recovery candidates = %#v, want only remaining C candidate", candidates)
	}
}

func TestSignedRevocationRemovesRelayCopyButAllowsNewerRotation(t *testing.T) {
	source, _ := newIdentity()
	relay := newStore()
	sourceState := newStore()
	payloadA := map[string]json.RawMessage{"access_token": json.RawMessage(`"old"`)}
	old, ok := sourceState.localCredChange(source, "g1", "115 Open", "/storage/115", payloadA, 1)
	if !ok {
		t.Fatal("source candidate was not recorded")
	}
	if !relay.mergeCred(old) {
		t.Fatal("relay did not accept initial source candidate")
	}
	rev, ok := sourceState.revokeOwnCred(source, "g1", "/storage/115", 2)
	if !ok || !rev.verify() {
		t.Fatal("source revocation was not signed")
	}
	if !relay.mergeRevocation(rev) {
		t.Fatal("relay did not accept source revocation")
	}
	if _, ok := relay.getCredForSource("g1", source.NodeID, "/storage/115"); ok {
		t.Fatal("relay retained revoked candidate")
	}
	if relay.mergeCred(old) {
		t.Fatal("relay accepted revoked candidate replay")
	}

	payloadB := map[string]json.RawMessage{"access_token": json.RawMessage(`"rotated"`)}
	newer, ok := sourceState.localCredChange(source, "g1", "115 Open", "/storage/115", payloadB, 3)
	if !ok || newer.Version <= rev.Version {
		t.Fatalf("source rotation did not advance beyond revocation: %#v", newer)
	}
	if !relay.mergeCred(newer) {
		t.Fatal("relay rejected newer credential rotation after revocation")
	}
}

// A signed candidate is relayable even when the relay has no local health proof.
// Otherwise a hub between two NAT'd nodes black-holes a good source's pair and
// recovery cannot converge. Local health remains required only for activation.
func TestCanOfferCred(t *testing.T) {
	id1, _ := newIdentity() // self
	id2, _ := newIdentity() // peer
	admin, _ := newIdentity()
	s := newStore()
	s.setGroups(admin, []group{{
		ID:      "g1",
		Members: []member{{NodeID: id1.NodeID, MountPath: "/local"}, {NodeID: id2.NodeID, MountPath: "/peer"}},
	}}, 1)
	m := &Manager{id: id1, state: s}

	if m.canOfferCred(nil) {
		t.Fatal("a nil record is never offerable")
	}

	// A peer-authored signed candidate must traverse this node unchanged.
	payload := map[string]json.RawMessage{"refresh_token": json.RawMessage(`"peer"`)}
	peer := &credRecord{
		GroupID:      "g1",
		OriginDriver: "115 Open",
		Payload:      payload,
		CredHash:     credHash(payload),
		Version:      1,
		Origin:       id2.NodeID,
		OriginPub:    id2.Pub,
		OriginMount:  "/peer",
	}
	peer.Sig = id2.sign(peer.signingBytes())
	if !m.canOfferCred(peer) {
		t.Fatal("an authorized peer candidate must be relayed without local proof")
	}

	// our own record, but no group/healthy mount -> not offerable.
	own := &credRecord{GroupID: "g1", Origin: id1.NodeID}
	if m.canOfferCred(own) {
		t.Fatal("our own record must NOT be offered when the token is not proven healthy")
	}
}

// This is an end-to-end backend regression for the exact missed transition:
// a peer candidate may arrive after the local mount is already invalid. It uses
// the registered production Local driver, real op storage persistence, and the
// same isolated probe + UpdateStorage path used in production -- not a mock
// driver or a mocked callback. The 115-specific three-node acceptance remains
// a separate live test because it must use a real operator-provided pair.
func TestReceivedCandidateRecoversInvalidMountWithRealDriver(t *testing.T) {
	dbName := fmt.Sprintf("file:cluster-recovery-%d?mode=memory&cache=shared", time.Now().UnixNano())
	database, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)

	oldRoot := t.TempDir()
	mount := fmt.Sprintf("/cluster-recovery-%d", time.Now().UnixNano())
	storage := model.Storage{
		Driver:    "Local",
		MountPath: mount,
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, oldRoot),
	}
	id, err := op.CreateStorage(context.Background(), storage)
	if err != nil {
		t.Fatalf("create real local storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })

	local, _ := newIdentity()
	peer, _ := newIdentity()
	admin, _ := newIdentity()
	state := newStore()
	state.setGroups(admin, []group{{
		ID: "g1",
		Members: []member{
			{NodeID: local.NodeID, MountPath: mount},
			{NodeID: peer.NodeID, MountPath: "/peer"},
		},
	}}, 1)
	manager := &Manager{
		dir:      t.TempDir(),
		id:       local,
		state:    state,
		conns:    newConnRegistry(),
		cfgStore: &configStore{cfg: Config{Enabled: true, Key: "test-key", ApplyRemote: true}},
	}

	d, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatalf("get created storage: %v", err)
	}
	// Model the persisted outcome of a real 401 before a peer has a replacement.
	d.GetStorage().SetStatus("token invalid")
	payload := map[string]json.RawMessage{"access_token": json.RawMessage(`"peer-pair"`)}
	candidate := &credRecord{
		GroupID:      "g1",
		OriginDriver: "Local",
		Fields:       []string{"access_token"},
		Payload:      payload,
		CredHash:     credHash(payload),
		Version:      1,
		Origin:       peer.NodeID,
		OriginPub:    peer.Pub,
		OriginMount:  "/peer",
		UpdatedAt:    now(),
	}
	candidate.Sig = peer.sign(candidate.signingBytes())
	candidate.MountSig = peer.sign(candidate.mountSigningBytes())
	if merged := manager.absorbCreds([]*credRecord{candidate}); len(merged) != 1 {
		t.Fatalf("candidate merge = %#v, want one accepted record", merged)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := op.GetStorageByMountPath(mount)
		if getErr == nil && current.GetStorage().Status == op.WORK && state.candidateHealthy("g1", candidate.CredHash, now()) {
			for _, event := range manager.eventList() {
				if event.Kind == "apply" && event.GroupID == "g1" {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	current, _ := op.GetStorageByMountPath(mount)
	t.Fatalf("received candidate did not activate invalid mount: %#v", current.GetStorage())
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
	m := &Manager{dir: t.TempDir(), id: localID, state: s, cfgStore: &configStore{cfg: Config{ApplyRemote: false}}}

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
	if _, ok := s.getCredForSource("g1", memberID.NodeID, "/115"); ok {
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
