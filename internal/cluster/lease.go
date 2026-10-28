package cluster

import (
	"encoding/binary"
	"sort"
	"strconv"
	"time"
)

// ----------------------------------------------------------------------------
// Refresher lease
//
// 115's refresh_token is single use: whoever refreshes kills every other copy of
// the pair until the new one arrives. So exactly one member per group, the
// refresher, may call the refresh endpoint. The role is a signed, per-group,
// last-writer-wins lease:
//
//   - the holder renews it periodically (Seq grows within an Epoch);
//   - a lease nobody renewed for TTL expires; the next member in the
//     deterministic takeover order claims it with Epoch+1, after first pulling
//     the latest credential so it never refreshes with a stale refresh_token;
//   - dominance is (Epoch, Seq, Holder), so simultaneous takers converge on the
//     larger node id and the loser stands down on receipt.
//
// Expiry is judged on the receiver's own clock from the moment it last saw the
// dominating lease (seenAt + TTLSec), never from a timestamp in the document, so
// clock skew between nodes cannot make a live lease look dead or vice versa.
// ----------------------------------------------------------------------------

// refreshSummary is the holder's last refresh attempt, carried in its lease so
// every node (and the UI) can show who refreshed last and how it went.
type refreshSummary struct {
	Time      int64  `json:"time"`
	Mount     string `json:"mount"`
	Trigger   string `json:"trigger"`
	Outcome   string `json:"outcome"`
	ErrCode   int64  `json:"err_code,omitempty"`
	ExpiresIn int64  `json:"expires_in,omitempty"`
}

type refresherLease struct {
	GroupID   string          `json:"group_id"`
	Holder    string          `json:"holder"`
	HolderPub []byte          `json:"holder_pub"`
	Epoch     uint64          `json:"epoch"`
	Seq       uint64          `json:"seq"`
	TTLSec    int64           `json:"ttl_sec"` // 0 = released
	IssuedAt  int64           `json:"issued_at"`
	Last      *refreshSummary `json:"last,omitempty"`
	Sig       []byte          `json:"sig"`
}

func (l *refresherLease) signingBytes() []byte {
	b := []byte("openlist/cluster/refresher-lease/v1\x00")
	b = append(b, l.GroupID...)
	b = append(b, 0)
	b = append(b, l.Holder...)
	b = append(b, 0)
	var n [8]byte
	for _, v := range []uint64{l.Epoch, l.Seq, uint64(l.TTLSec), uint64(l.IssuedAt)} {
		binary.LittleEndian.PutUint64(n[:], v)
		b = append(b, n[:]...)
	}
	if l.Last != nil {
		b = append(b, 1)
		b = strconv.AppendInt(b, l.Last.Time, 10)
		b = append(b, 0)
		b = append(b, l.Last.Mount...)
		b = append(b, 0)
		b = append(b, l.Last.Trigger...)
		b = append(b, 0)
		b = append(b, l.Last.Outcome...)
		b = append(b, 0)
		b = strconv.AppendInt(b, l.Last.ErrCode, 10)
		b = append(b, 0)
		b = strconv.AppendInt(b, l.Last.ExpiresIn, 10)
	}
	return b
}

func (l *refresherLease) verify() bool {
	return l != nil && l.GroupID != "" && l.Holder != "" && l.TTLSec >= 0 &&
		nodeIDFromPub(l.HolderPub) == l.Holder && verifySig(l.HolderPub, l.signingBytes(), l.Sig)
}

func (l *refresherLease) dominates(o *refresherLease) bool {
	if o == nil {
		return true
	}
	if l.Epoch != o.Epoch {
		return l.Epoch > o.Epoch
	}
	if l.Seq != o.Seq {
		return l.Seq > o.Seq
	}
	return l.Holder > o.Holder
}

// leaseAuthorizedByGroups: only a member of the group may hold its lease.
func leaseAuthorizedByGroups(groups []group, l *refresherLease) bool {
	if !l.verify() {
		return false
	}
	for _, g := range groups {
		if g.ID != l.GroupID {
			continue
		}
		for _, mem := range g.Members {
			if mem.NodeID == l.Holder {
				return true
			}
		}
	}
	return false
}

// leaseEntry is the dominating lease we know plus when we last heard it renewed.
type leaseEntry struct {
	lease  *refresherLease
	seenAt time.Time
}

// valid reports whether the lease currently designates a holder.
func (e *leaseEntry) valid(at time.Time) bool {
	if e == nil || e.lease == nil || e.lease.TTLSec <= 0 {
		return false
	}
	return at.Sub(e.seenAt) < time.Duration(e.lease.TTLSec)*time.Second
}

// groupPolicy is the admin's per-group refresher choice, a small signed LWW
// document of its own (not part of the group document, whose signed bytes older
// nodes recompute and would reject if it grew a field).
type groupPolicy struct {
	GroupID   string `json:"group_id"`
	Preferred string `json:"preferred"` // node id, "" = no preference
	Version   uint64 `json:"version"`
	Origin    string `json:"origin"`
	OriginPub []byte `json:"origin_pub"`
	UpdatedAt int64  `json:"updated_at"`
	Sig       []byte `json:"sig"`
}

func (p *groupPolicy) signingBytes() []byte {
	b := []byte("openlist/cluster/group-policy/v1\x00")
	b = append(b, p.GroupID...)
	b = append(b, 0)
	b = append(b, p.Preferred...)
	b = append(b, 0)
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], p.Version)
	b = append(b, n[:]...)
	b = append(b, 0)
	b = append(b, p.Origin...)
	return b
}

func (p *groupPolicy) verify() bool {
	return p != nil && p.GroupID != "" && p.Version > 0 &&
		nodeIDFromPub(p.OriginPub) == p.Origin && verifySig(p.OriginPub, p.signingBytes(), p.Sig)
}

func (p *groupPolicy) dominates(o *groupPolicy) bool {
	if o == nil {
		return true
	}
	if p.Version != o.Version {
		return p.Version > o.Version
	}
	return p.Origin > o.Origin
}

// ---- store accessors ----

// leaseOf returns the dominating lease entry for a group.
func (s *store) leaseOf(groupID string) *leaseEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e := s.leases[groupID]; e != nil {
		cp := *e
		return &cp
	}
	return nil
}

// mergeLease integrates a verified, authorized lease. seenAt is refreshed
// whenever the dominating lease (or an identical renewal) is heard again.
// Returns true when the lease replaced ours (a new Seq/Epoch/Holder).
func (s *store) mergeLease(l *refresherLease, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.leases[l.GroupID]
	if cur != nil && !l.dominates(cur.lease) {
		return false
	}
	cp := *l
	s.leases[l.GroupID] = &leaseEntry{lease: &cp, seenAt: at}
	return true
}

// issueLease authors and stores this node's own lease for a group.
func (s *store) issueLease(id *identity, groupID string, epoch, seq uint64, ttlSec int64, last *refreshSummary, at time.Time) *refresherLease {
	l := &refresherLease{
		GroupID: groupID, Holder: id.NodeID, HolderPub: id.Pub,
		Epoch: epoch, Seq: seq, TTLSec: ttlSec, IssuedAt: at.Unix(),
	}
	if last != nil {
		cp := *last
		l.Last = &cp
	}
	l.Sig = id.sign(l.signingBytes())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases[groupID] = &leaseEntry{lease: l, seenAt: at}
	cp := *l
	return &cp
}

func (s *store) leaseList() []*refresherLease {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*refresherLease, 0, len(s.leases))
	for _, e := range s.leases {
		cp := *e.lease
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupID < out[j].GroupID })
	return out
}

func (s *store) policyOf(groupID string) *groupPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p := s.policies[groupID]; p != nil {
		cp := *p
		return &cp
	}
	return nil
}

func (s *store) policyList() []*groupPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*groupPolicy, 0, len(s.policies))
	for _, p := range s.policies {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupID < out[j].GroupID })
	return out
}

func (s *store) mergePolicy(p *groupPolicy) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observe(p.Version)
	if cur := s.policies[p.GroupID]; cur != nil && !p.dominates(cur) {
		return false
	}
	cp := *p
	s.policies[p.GroupID] = &cp
	return true
}

// setPolicy authors a new preferred-refresher policy.
func (s *store) setPolicy(id *identity, groupID, preferred string, at int64) *groupPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lamport++
	p := &groupPolicy{GroupID: groupID, Preferred: preferred, Version: s.lamport, Origin: id.NodeID, OriginPub: id.Pub, UpdatedAt: at}
	p.Sig = id.sign(p.signingBytes())
	s.policies[groupID] = p
	cp := *p
	return &cp
}

// pruneLeases drops leases and policies of groups that no longer exist.
func (s *store) pruneLeases() {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := make(map[string]struct{}, len(s.groups.Groups))
	for _, g := range s.groups.Groups {
		live[g.ID] = struct{}{}
	}
	for id := range s.leases {
		if _, ok := live[id]; !ok {
			delete(s.leases, id)
		}
	}
	for id := range s.policies {
		if _, ok := live[id]; !ok {
			delete(s.policies, id)
		}
	}
}

func timeNow() time.Time { return time.Now() }
