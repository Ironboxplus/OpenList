package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
	open115 "github.com/OpenListTeam/OpenList/v4/drivers/115_open"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/local"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestProvider401ClassificationKeepsRefreshThrottleRetryable(t *testing.T) {
	tests := []struct {
		name     string
		code     int64
		terminal bool
	}{
		{name: "refresh throttle", code: sdk.CodeRefreshFrequently, terminal: false},
		{name: "dead refresh token", code: sdk.CodeRefreshTokenError, terminal: true},
		{name: "invalid access after refresh", code: 40140126, terminal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isProvider401(&sdk.Error{Code: tt.code, Message: tt.name})
			if got != tt.terminal {
				t.Fatalf("isProvider401(%d) = %v, want %v", tt.code, got, tt.terminal)
			}
		})
	}
	if !isProviderRefreshThrottle(&sdk.Error{Code: sdk.CodeRefreshFrequently}) {
		t.Fatal("40140117 was not classified for delayed retry")
	}
	if isProviderRefreshThrottle(&sdk.Error{Code: sdk.CodeRefreshTokenError}) {
		t.Fatal("40140120 was incorrectly classified as retryable")
	}
}

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

// additionField decodes one string field out of a storage's Addition JSON.
// Windows temp-dir paths contain backslashes, which %q/json both escape, so a
// plain strings.Contains(addition, rawPath) check is unreliable cross-platform
// -- decode and compare the real value instead.
func additionField(t *testing.T, additionJSON, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(additionJSON), &m); err != nil {
		t.Fatalf("decode addition JSON %q: %v", additionJSON, err)
	}
	v, _ := m[key].(string)
	return v
}

// applyCredRecordToMount is the sole gate deciding whether a candidate may
// touch production storage. A WORK mount is not proof its pair is still
// accepted by the provider — 115's refresh_token rotates on use, so a peer's
// successful refresh can kill this mount's own pair before this node ever
// sees a local 401 — so WORK must not by itself block adoption of a
// peer-authored candidate. It must still block a self-authored record
// (reachable only via a relay echo) and must still require the temporary
// driver probe to actually pass before committing anything.
func TestApplyCredRecordToMountReturnValues(t *testing.T) {
	dbName := fmt.Sprintf("file:apply-cred-matrix-%d?mode=memory&cache=shared", time.Now().UnixNano())
	database, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)

	localID, _ := newIdentity()
	peerID, _ := newIdentity()
	adminID, _ := newIdentity()

	type want struct{ attempted, success, terminal, retryable bool }
	tests := []struct {
		name string
		// The Local driver's typed Addition struct has no credential-shaped
		// field, so an injected "access_token" key never survives
		// initStorage's marshal-through-the-typed-struct round trip (see
		// op.saveDriverStorage) -- it is silently dropped on every commit,
		// including the very first op.CreateStorage. "creds differ" is
		// therefore modeled as a non-empty payload (applyCreds always reports
		// changed=true for a brand new key), and "creds identical" as an
		// empty payload (applyCreds short-circuits on an empty map before it
		// ever looks at the current Addition). This keeps the case selection
		// independent of that round-trip quirk while still exercising the
		// exact changed/unchanged branch in applyCredRecordToMount.
		initialWork  bool
		selfOrigin   bool
		emptyPayload bool
		probeErr     error
		want         want
		wantProbes   int32
	}{
		{
			name:        "WORK peer-origin differing creds probe succeeds adopts",
			initialWork: true,
			want:        want{true, true, false, false},
			wantProbes:  1,
		},
		{
			name:        "WORK self-origin differing creds blocked before probe",
			initialWork: true,
			selfOrigin:  true,
			want:        want{false, false, false, false},
			wantProbes:  0,
		},
		{
			name:         "WORK peer-origin empty payload no churn",
			initialWork:  true,
			emptyPayload: true,
			want:         want{false, false, false, false},
			wantProbes:   0,
		},
		{
			name:        "WORK peer-origin differing creds probe terminal 401",
			initialWork: true,
			probeErr:    &sdk.Error{Code: sdk.CodeRefreshTokenError, Message: "dead"},
			want:        want{true, false, true, false},
			wantProbes:  1,
		},
		{
			name:        "WORK peer-origin differing creds probe throttle 40140117",
			initialWork: true,
			probeErr:    &sdk.Error{Code: sdk.CodeRefreshFrequently, Message: "slow down"},
			want:        want{true, false, false, true},
			wantProbes:  1,
		},
		{
			name:       "non-WORK peer-origin differing creds probe succeeds adopts",
			want:       want{true, true, false, false},
			wantProbes: 1,
		},
		{
			name:       "non-WORK peer-origin differing creds probe terminal 401",
			probeErr:   &sdk.Error{Code: sdk.CodeRefreshTokenError, Message: "dead"},
			want:       want{true, false, true, false},
			wantProbes: 1,
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mount := fmt.Sprintf("/apply-cred-matrix-%d-%d", time.Now().UnixNano(), i)
			dir := t.TempDir()
			id, err := op.CreateStorage(context.Background(), model.Storage{
				Driver:    "Local",
				MountPath: mount,
				Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, dir),
			})
			if err != nil {
				t.Fatalf("create storage: %v", err)
			}
			t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })

			d, err := op.GetStorageByMountPath(mount)
			if err != nil {
				t.Fatalf("get storage: %v", err)
			}
			if !tc.initialWork {
				d.GetStorage().SetStatus("token invalid")
			}
			beforeModified := d.GetStorage().Modified

			state := newStore()
			state.setGroups(adminID, []group{{
				ID: "g1",
				Members: []member{
					{NodeID: localID.NodeID, MountPath: mount},
					{NodeID: peerID.NodeID, MountPath: "/peer"},
				},
			}}, 1)

			var probes atomic.Int32
			manager := &Manager{
				dir:      t.TempDir(),
				id:       localID,
				state:    state,
				conns:    newConnRegistry(),
				cfgStore: &configStore{cfg: Config{Enabled: true, Key: "test-key", ApplyRemote: true}},
				probeCredentialFn: func(context.Context, model.Storage, string) error {
					probes.Add(1)
					return tc.probeErr
				},
			}

			origin, originMount := peerID, "/peer"
			if tc.selfOrigin {
				origin, originMount = localID, mount
			}
			payload := map[string]json.RawMessage{"access_token": json.RawMessage(`"new-token"`)}
			if tc.emptyPayload {
				payload = map[string]json.RawMessage{}
			}
			candidate := &credRecord{
				GroupID:      "g1",
				OriginDriver: "Local",
				Fields:       []string{"access_token"},
				Payload:      payload,
				CredHash:     credHash(payload),
				Version:      1,
				Origin:       origin.NodeID,
				OriginPub:    origin.Pub,
				OriginMount:  originMount,
				UpdatedAt:    now(),
			}

			attempted, success, terminal, retryable := manager.applyCredRecordToMount(candidate, mount)
			if attempted != tc.want.attempted || success != tc.want.success || terminal != tc.want.terminal || retryable != tc.want.retryable {
				t.Fatalf("applyCredRecordToMount = (attempted=%v success=%v terminal=%v retryable=%v), want %+v",
					attempted, success, terminal, retryable, tc.want)
			}
			if got := probes.Load(); got != tc.wantProbes {
				t.Fatalf("probe called %d times, want %d", got, tc.wantProbes)
			}

			after, err := op.GetStorageByMountPath(mount)
			if err != nil {
				t.Fatalf("get storage after: %v", err)
			}
			afterModified := after.GetStorage().Modified
			// op.UpdateStorage unconditionally stamps a fresh Modified on every
			// commit, independent of which Addition fields the driver's typed
			// struct happens to recognize -- a reliable, driver-agnostic signal
			// for "was this row actually written."
			if tc.want.success {
				if !afterModified.After(beforeModified) {
					t.Fatalf("successful adoption did not commit a new storage generation (Modified unchanged: %v)", afterModified)
				}
				if got := additionField(t, after.GetStorage().Addition, "root_folder_path"); got != dir {
					t.Fatalf("successful adoption dropped the node-local root_folder_path: got %q, want %q", got, dir)
				}
			} else if !afterModified.Equal(beforeModified) {
				t.Fatalf("unsuccessful/blocked attempt still committed a new storage generation: before=%v after=%v", beforeModified, afterModified)
			}
		})
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

	// Poll the SQLite row, not op.GetStorageByMountPath's live driver.Driver:
	// recovery replaces the mount from a background goroutine, so reading the
	// shared *model.Storage from here is an unsynchronized read of state that
	// production legitimately mutates concurrently.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := db.GetStorageByMountPath(mount)
		if getErr == nil && current.Status == op.WORK && state.candidateHealthy("g1", candidate.CredHash, now()) {
			for _, event := range manager.eventList() {
				if event.Kind == "apply" && event.GroupID == "g1" {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	current, _ := db.GetStorageByMountPath(mount)
	t.Fatalf("received candidate did not activate invalid mount: %#v", current)
}

// A WORK mount must actively adopt a peer's newer credential through the same
// full recovery pipeline (absorbCreds -> enqueueCandidate -> runCandidateRecovery)
// used for an already-invalid mount, not just via a direct applyCredRecordToMount
// call. This is the regression for Bug 2: node B never fails locally (its old
// refresh_token still reads WORK), but A's rotation already killed it on the
// provider side, and B must not wait for its own 401 before taking A's pair.
// It uses the real Local driver and real op storage persistence, mirroring
// TestReceivedCandidateRecoversInvalidMountWithRealDriver but starting the
// mount at WORK. Node-local fields (root_folder_path) must survive the swap.
func TestReceivedCandidateIsAdoptedByWorkMountWithRealDriver(t *testing.T) {
	dbName := fmt.Sprintf("file:cluster-work-adopt-%d?mode=memory&cache=shared", time.Now().UnixNano())
	database, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)

	oldRoot := t.TempDir()
	mount := fmt.Sprintf("/cluster-work-adopt-%d", time.Now().UnixNano())
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

	// Confirm the fixture actually starts WORK — the whole point of this test.
	d, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatalf("get created storage: %v", err)
	}
	if d.GetStorage().Status != op.WORK {
		t.Fatalf("fixture storage status = %q, want %q before the peer candidate arrives", d.GetStorage().Status, op.WORK)
	}
	beforeModified := d.GetStorage().Modified

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

	payload := map[string]json.RawMessage{"access_token": json.RawMessage(`"peer-rotated-pair"`)}
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
	// absorbCreds is what a received "push"/"reply" sync message drives in
	// production; it enqueues the candidate for every local mount in the group
	// regardless of that mount's current status.
	if merged := manager.absorbCreds([]*credRecord{candidate}); len(merged) != 1 {
		t.Fatalf("candidate merge = %#v, want one accepted record", merged)
	}

	// The Local driver's typed Addition struct has no credential-shaped field,
	// so "access_token" never survives initStorage's marshal-through-the-typed
	// -struct round trip (see op.saveDriverStorage) -- adoption is observed
	// the same way TestReceivedCandidateRecoversInvalidMountWithRealDriver
	// observes it: a fresh storage generation (Modified advanced) plus the
	// cluster's own proof that this exact candidate is now the healthy one.
	//
	// Polling reads db.GetStorageByMountPath (a fresh row from SQLite) rather
	// than op.GetStorageByMountPath (the live driver.Driver pointer that the
	// recovery goroutine concurrently mutates in place via SetStorage/
	// MustSaveDriverStorage). Reading the live pointer here races under -race
	// with that goroutine's write -- a pre-existing pattern shared by
	// TestReceivedCandidateRecoversInvalidMountWithRealDriver, which is
	// flaky under -race for the same reason (reproduced independently while
	// verifying this change; not introduced by it).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := db.GetStorageByMountPath(mount)
		if getErr == nil && current.Status == op.WORK &&
			current.Modified.After(beforeModified) &&
			state.candidateHealthy("g1", candidate.CredHash, now()) {
			if got := additionField(t, current.Addition, "root_folder_path"); got != oldRoot {
				t.Fatalf("adoption dropped the node-local root_folder_path: got %q, want %q", got, oldRoot)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	current, _ := db.GetStorageByMountPath(mount)
	t.Fatalf("WORK mount did not adopt the peer's rotated credential: %#v", current)
}

// A candidate that fails the temporary-driver probe must never touch
// production storage, even through the full async recovery queue (not just a
// direct applyCredRecordToMount call). UpdateStorage persists before it
// initializes, so a bug here would be a real durable corruption, not just a
// returned error.
func TestFailedProbeLeavesWorkMountStorageUntouched(t *testing.T) {
	dbName := fmt.Sprintf("file:cluster-work-probe-fail-%d?mode=memory&cache=shared", time.Now().UnixNano())
	database, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)

	oldRoot := t.TempDir()
	mount := fmt.Sprintf("/cluster-work-probe-fail-%d", time.Now().UnixNano())
	id, err := op.CreateStorage(context.Background(), model.Storage{
		Driver:    "Local",
		MountPath: mount,
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, oldRoot),
	})
	if err != nil {
		t.Fatalf("create real local storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })

	// initStorage round-trips Addition through the driver's own JSON
	// marshaling (see op.saveDriverStorage), so the durable baseline for "was
	// this row touched at all" is whatever actually landed after creation, not
	// the hand-written literal passed above.
	created, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatalf("get created storage: %v", err)
	}
	originalAddition := created.GetStorage().Addition

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
		stopCh:   make(chan struct{}),
		probeCredentialFn: func(context.Context, model.Storage, string) error {
			return &sdk.Error{Code: sdk.CodeRefreshTokenError, Message: "dead pair"}
		},
	}
	t.Cleanup(manager.Stop)

	payload := map[string]json.RawMessage{"access_token": json.RawMessage(`"doomed-pair"`)}
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

	// A terminal probe failure ends in a signed group-wide revocation of the
	// candidate (runCandidateRecovery's terminal branch calls revokePair, not
	// markCandidateFailed) — that tombstone is the queue's real termination
	// signal; there is no separate "done" event for a doomed candidate.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(state.revocationSnapshot()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(state.revocationSnapshot()) == 0 {
		t.Fatal("terminal probe failure did not revoke the doomed candidate")
	}

	current, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatalf("get storage after failed probe: %v", err)
	}
	if current.GetStorage().Status != op.WORK {
		t.Fatalf("failed probe changed a WORK mount's status to %q", current.GetStorage().Status)
	}
	// Exact equality (not just a substring check) proves the row was never
	// touched at all -- not even rewritten back to an equivalent value.
	if current.GetStorage().Addition != originalAddition {
		t.Fatalf("failed probe mutated production storage: got %q, want unchanged %q",
			current.GetStorage().Addition, originalAddition)
	}
	if strings.Contains(current.GetStorage().Addition, "doomed-pair") {
		t.Fatal("failed probe committed the doomed candidate's payload")
	}
}

// A live probe only proves a candidate is accepted by the provider right now;
// it says nothing about whether the candidate is actually newer than what
// this node already knows for the mount's current pair. 115's access_token
// carries its own TTL, so a pair that already lost a refresh_token rotation
// race can still probe clean for a while -- without a version check a WORK
// mount could regress onto that dead pair, and two WORK nodes could
// oscillate adopting each other's stale credentials. This exercises the
// guard in applyCredRecordToMount: a candidate is rejected before ever
// reaching the probe when this node's own catalogue already has an
// equal-or-newer record for the mount's current credential hash, and is
// otherwise let through unchanged (including when no catalog record exists
// yet, where the guard has no regression information and must stay
// permissive).
func TestApplyCredRecordToMountRejectsVersionRegression(t *testing.T) {
	dbName := fmt.Sprintf("file:apply-cred-regression-%d?mode=memory&cache=shared", time.Now().UnixNano())
	database, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)

	localID, _ := newIdentity()
	peerID, _ := newIdentity()
	adminID, _ := newIdentity()

	tests := []struct {
		name             string
		catalogVersion   uint64 // 0 = do not seed a matching catalog record
		candidateVersion uint64
		wantAttempted    bool
		wantProbes       int32
	}{
		{
			name:             "older candidate rejected before probe",
			catalogVersion:   5,
			candidateVersion: 3,
			wantAttempted:    false,
			wantProbes:       0,
		},
		{
			name:             "equal version rejected before probe",
			catalogVersion:   5,
			candidateVersion: 5,
			wantAttempted:    false,
			wantProbes:       0,
		},
		{
			name:             "newer candidate still probed and adopted",
			catalogVersion:   5,
			candidateVersion: 6,
			wantAttempted:    true,
			wantProbes:       1,
		},
		{
			name:             "no catalog record permits candidate",
			catalogVersion:   0,
			candidateVersion: 1,
			wantAttempted:    true,
			wantProbes:       1,
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mount := fmt.Sprintf("/apply-cred-regression-%d-%d", time.Now().UnixNano(), i)
			dir := t.TempDir()
			id, err := op.CreateStorage(context.Background(), model.Storage{
				Driver:    "Local",
				MountPath: mount,
				Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, dir),
			})
			if err != nil {
				t.Fatalf("create storage: %v", err)
			}
			t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })

			d, err := op.GetStorageByMountPath(mount)
			if err != nil {
				t.Fatalf("get storage: %v", err)
			}
			// Stamp a credential field directly onto the live storage's Addition.
			// The Local driver's typed Addition struct has no credential-shaped
			// field, so this could never survive an op.UpdateStorage round trip
			// (see op.saveDriverStorage) -- but applyCredRecordToMount reads the
			// storage struct as-is and never re-marshals it through the driver
			// before the regression check, so a direct field write faithfully
			// models what a real credentialed driver's Addition already contains.
			currentAddition := fmt.Sprintf(`{"root_folder_path":%q,"access_token":"current-pair"}`, dir)
			d.GetStorage().Addition = currentAddition

			state := newStore()
			state.setGroups(adminID, []group{{
				ID: "g1",
				Members: []member{
					{NodeID: localID.NodeID, MountPath: mount},
					{NodeID: peerID.NodeID, MountPath: "/peer"},
				},
			}}, 1)

			currentHash := credHash(extractCreds(currentAddition))
			if tc.catalogVersion != 0 {
				if !state.mergeCred(&credRecord{
					GroupID:      "g1",
					OriginDriver: "Local",
					Fields:       []string{"access_token"},
					Payload:      map[string]json.RawMessage{"access_token": json.RawMessage(`"current-pair"`)},
					CredHash:     currentHash,
					Version:      tc.catalogVersion,
					Origin:       localID.NodeID,
					OriginPub:    localID.Pub,
					OriginMount:  mount,
					UpdatedAt:    now(),
				}) {
					t.Fatalf("seed catalog record: mergeCred rejected it")
				}
			}

			var probes atomic.Int32
			manager := &Manager{
				dir:      t.TempDir(),
				id:       localID,
				state:    state,
				conns:    newConnRegistry(),
				cfgStore: &configStore{cfg: Config{Enabled: true, Key: "test-key", ApplyRemote: true}},
				probeCredentialFn: func(context.Context, model.Storage, string) error {
					probes.Add(1)
					return nil
				},
			}

			payload := map[string]json.RawMessage{"access_token": json.RawMessage(`"peer-pair"`)}
			candidate := &credRecord{
				GroupID:      "g1",
				OriginDriver: "Local",
				Fields:       []string{"access_token"},
				Payload:      payload,
				CredHash:     credHash(payload),
				Version:      tc.candidateVersion,
				Origin:       peerID.NodeID,
				OriginPub:    peerID.Pub,
				OriginMount:  "/peer",
				UpdatedAt:    now(),
			}

			attempted, success, terminal, retryable := manager.applyCredRecordToMount(candidate, mount)
			if attempted != tc.wantAttempted {
				t.Fatalf("attempted = %v, want %v (success=%v terminal=%v retryable=%v)", attempted, tc.wantAttempted, success, terminal, retryable)
			}
			if got := probes.Load(); got != tc.wantProbes {
				t.Fatalf("probe called %d times, want %d", got, tc.wantProbes)
			}
			if tc.wantAttempted && !success {
				t.Fatalf("expected the newer/unknown-version candidate to be adopted, got success=false (terminal=%v retryable=%v)", terminal, retryable)
			}
		})
	}
}

func TestRefreshThrottleRequeuesCandidateAfterCooldown(t *testing.T) {
	dbName := fmt.Sprintf("file:cluster-throttle-retry-%d?mode=memory&cache=shared", time.Now().UnixNano())
	database, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)

	mount := fmt.Sprintf("/cluster-throttle-retry-%d", time.Now().UnixNano())
	id, err := op.CreateStorage(context.Background(), model.Storage{
		Driver:    "Local",
		MountPath: mount,
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, t.TempDir()),
	})
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
	var probes atomic.Int32
	manager := &Manager{
		dir:                 t.TempDir(),
		id:                  local,
		state:               state,
		conns:               newConnRegistry(),
		cfgStore:            &configStore{cfg: Config{Enabled: true, Key: "test-key", ApplyRemote: true}},
		stopCh:              make(chan struct{}),
		candidateRetryDelay: 20 * time.Millisecond,
		probeCredentialFn: func(context.Context, model.Storage, string) error {
			if probes.Add(1) == 1 {
				return &sdk.Error{Code: sdk.CodeRefreshFrequently, Message: "refresh frequently"}
			}
			return nil
		},
	}
	t.Cleanup(manager.Stop)

	d, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatalf("get created storage: %v", err)
	}
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

	// Poll the SQLite row rather than the live driver.Driver — see the note in
	// TestReceivedCandidateRecoversInvalidMountWithRealDriver.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := db.GetStorageByMountPath(mount)
		if getErr == nil && current.Status == op.WORK && probes.Load() >= 2 {
			if got := len(state.revocationSnapshot()); got != 0 {
				t.Fatalf("retryable throttle created %d revocation(s)", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, _ := db.GetStorageByMountPath(mount)
	t.Fatalf("throttled candidate was not retried: probes=%d storage=%#v", probes.Load(), current)
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
