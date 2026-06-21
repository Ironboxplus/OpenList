package cluster

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func mkCfg(mount, addition string) syncableStorage {
	return fromModel(&model.Storage{MountPath: mount, Driver: "115 Open", Addition: addition})
}

func TestLocalChangeIdempotent(t *testing.T) {
	s := newStore()
	id, _ := newIdentity()
	cfg := mkCfg("/115", `{"token":"v1"}`)

	r1, ok := s.localChange(id, cfg, 1)
	if !ok || r1 == nil {
		t.Fatal("first change should produce a record")
	}
	v1 := r1.Version

	// Same content again => no-op, no version bump ("same token, don't change").
	if r2, ok := s.localChange(id, cfg, 2); ok || r2 != nil {
		t.Fatal("identical config must be a no-op")
	}
	if cur, _ := s.get("/115"); cur.Version != v1 {
		t.Fatalf("version churned on identical config: %d != %d", cur.Version, v1)
	}

	// Changed content => new version.
	r3, ok := s.localChange(id, mkCfg("/115", `{"token":"v2"}`), 3)
	if !ok || r3.Version <= v1 {
		t.Fatalf("changed config must bump version (%d should be > %d)", r3.Version, v1)
	}
}

func TestRecordVerify(t *testing.T) {
	s := newStore()
	id, _ := newIdentity()
	r, _ := s.localChange(id, mkCfg("/a", `{"t":"1"}`), 1)
	if !r.verify() {
		t.Fatal("self-produced record must verify")
	}
	// Tamper with the content hash.
	bad := *r
	bad.ContentHash = "deadbeef"
	if bad.verify() {
		t.Fatal("record with mismatched content hash must not verify")
	}
	// Forge origin (claim a different node id without its key).
	forged := *r
	forged.Origin = "SOMEONEELSE"
	if forged.verify() {
		t.Fatal("record whose origin != hash(pubkey) must not verify")
	}
}

func TestMergeLWWAndIdempotency(t *testing.T) {
	// Two nodes author the same mount; higher Lamport version wins.
	nodeA, _ := newIdentity()
	nodeB, _ := newIdentity()

	sa := newStore()
	rA, _ := sa.localChange(nodeA, mkCfg("/m", `{"t":"A"}`), 1)

	sb := newStore()
	// B is ahead on the Lamport clock.
	sb.lamport = 5
	rB, _ := sb.localChange(nodeB, mkCfg("/m", `{"t":"B"}`), 1)

	// On a third node, apply A then B: B (higher version) must win.
	s := newStore()
	if got := s.merge(rA); got != mergeApplied {
		t.Fatalf("first merge = %v, want applied", got)
	}
	if got := s.merge(rB); got != mergeApplied {
		t.Fatalf("higher-version merge = %v, want applied", got)
	}
	cur, _ := s.get("/m")
	if cur.Origin != nodeB.NodeID {
		t.Fatal("LWW: higher Lamport version (B) should win")
	}

	// Re-merging A (lower version) is ignored.
	if got := s.merge(rA); got != mergeIgnored {
		t.Fatalf("stale merge = %v, want ignored", got)
	}

	// Merging an identical-content record is ignored (idempotent).
	dupB := *rB
	if got := s.merge(&dupB); got != mergeIgnored {
		t.Fatalf("duplicate-content merge = %v, want ignored", got)
	}
}

func TestMergeTombstone(t *testing.T) {
	id, _ := newIdentity()
	s := newStore()
	s.localChange(id, mkCfg("/d", `{"t":"1"}`), 1)
	del, ok := s.localDelete(id, "/d", 2)
	if !ok {
		t.Fatal("delete should produce a tombstone")
	}

	other := newStore()
	other.merge(mustRecord(t, id, "/d", `{"t":"1"}`, 1))
	if got := other.merge(del); got != mergeTombstone {
		t.Fatalf("tombstone merge = %v, want tombstone", got)
	}
	cur, _ := other.get("/d")
	if !cur.Tombstone {
		t.Fatal("record should be tombstoned after merge")
	}
}

func mustRecord(t *testing.T, id *identity, mount, addition string, now int64) *record {
	t.Helper()
	s := newStore()
	r, ok := s.localChange(id, mkCfg(mount, addition), now)
	if !ok {
		t.Fatal("failed to build record")
	}
	return r
}

func TestApplyToPreservesLocalRuntimeFields(t *testing.T) {
	cfg := mkCfg("/x", `{"t":"shared"}`)
	dst := &model.Storage{ID: 42, Status: "work", Addition: `{"t":"old"}`}
	cfg.applyTo(dst)
	if dst.ID != 42 || dst.Status != "work" {
		t.Fatal("applyTo must preserve local ID/Status")
	}
	if dst.Addition != `{"t":"shared"}` {
		t.Fatalf("applyTo must overwrite Addition, got %q", dst.Addition)
	}
}
