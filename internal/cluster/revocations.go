package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
)

// ----------------------------------------------------------------------------
// Revocation digest and pruning
//
// A revocation is a signed tombstone for one complete credential pair that the
// provider rejected. Announces used to attach the full set to every frame (about
// 10 KB with 28 entries) and every reply. Now announce/hello carry only a
// digest; the receiver pulls the set when the digests differ.
//
// Pruning keeps the set from growing forever without letting a dead pair come
// back. A revocation R (pair P, authored at R.UpdatedAt) is prunable once
//
//  1. it is older than the retention window, AND
//  2. the group holds a live credential newer than R.UpdatedAt (so the group is
//     not left relying on P's lineage).
//
// Pruning raises revFloor[group] to R.UpdatedAt and mergeCred refuses any record
// authored at or before the floor. P was authored before it was ever rejected,
// so P.UpdatedAt <= R.UpdatedAt (modulo clock skew, which is negligible against a
// multi-day window): P stays out even though its tombstone is gone, on every
// node, in any arrival order. We deliberately use the signed wall-clock
// UpdatedAt rather than the Lamport version: a reporter that never observed P's
// record can mint a revocation whose Lamport version is lower than P's, so the
// Lamport order proves nothing about P.
// ----------------------------------------------------------------------------

const defaultRevocationRetentionSec int64 = 14 * 24 * 3600

func (s *store) retentionLocked() int64 {
	if s.revRetentionSec > 0 {
		return s.revRetentionSec
	}
	return defaultRevocationRetentionSec
}

// revPrunableLocked implements the prune condition. Caller holds s.mu.
func (s *store) revPrunableLocked(rev *credRevocation) bool {
	if s.clock()-rev.UpdatedAt <= s.retentionLocked() {
		return false
	}
	for _, c := range s.creds[rev.GroupID] {
		if c.UpdatedAt > rev.UpdatedAt && c.CredHash != rev.CredHash {
			return true
		}
	}
	return false
}

// dropCredHashLocked removes every candidate carrying the pair. Caller holds s.mu.
func (s *store) dropCredHashLocked(groupID, hash string) {
	byOrigin := s.creds[groupID]
	for k, c := range byOrigin {
		if c.CredHash == hash {
			delete(byOrigin, k)
		}
	}
	if len(byOrigin) == 0 {
		delete(s.creds, groupID)
	}
}

// pruneRevocations removes prunable revocations, raising the group floors. It
// returns how many were pruned.
func (s *store) pruneRevocations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for gid, byHash := range s.revocations {
		for h, rev := range byHash {
			if !s.revPrunableLocked(rev) {
				continue
			}
			if rev.UpdatedAt > s.revFloor[gid] {
				s.revFloor[gid] = rev.UpdatedAt
			}
			delete(byHash, h)
			n++
		}
		if len(byHash) == 0 {
			delete(s.revocations, gid)
		}
	}
	return n
}

// revocationDigest summarises the revocation set: identical sets give identical
// digests on every node. Each entry contributes (group, pair hash, version,
// origin) so a dominating replacement also changes it.
func revocationDigest(revs []*credRevocation) string {
	if len(revs) == 0 {
		return ""
	}
	lines := make([]string, 0, len(revs))
	for _, r := range revs {
		lines = append(lines, r.GroupID+"\x00"+r.CredHash+"\x00"+strconv.FormatUint(r.Version, 10)+"\x00"+r.Origin)
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (s *store) revocationDigest() string {
	return revocationDigest(s.revocationSnapshot())
}
