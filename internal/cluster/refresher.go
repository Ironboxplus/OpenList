package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// ----------------------------------------------------------------------------
// Single refresher per group
//
// Only the lease holder may call the provider's refresh endpoint (the driver asks
// MayRefresh before every automatic refresh). The holder refreshes at 80% of the
// access token's lifetime and the normal token-valid publication then pushes the
// new pair to the group, so members never hit an expired token. A member whose
// token is rejected pulls, and if no newer pair arrives in time it asks the
// holder (refresh-request). A holder that vanishes loses the lease after its TTL
// and the next member in takeover order claims it, pulling the latest pair first.
// ----------------------------------------------------------------------------

const (
	// refreshDriverName is the only driver whose refresh_token is single-use and
	// which implements op.TokenRefresher; other groups take no part in leases.
	refreshDriverName = "115 Open"
	// proactiveFraction is the share of the access token lifetime after which
	// the holder refreshes ahead of expiry.
	proactiveFraction = 0.8
	// settleDelay: a freshly claimed lease only starts refreshing after this
	// long, so a simultaneous taker's dominating lease can reach us first.
	settleDelay = 2 * time.Second
	// maxRequestLog bounds the recent refresh-request log.
	maxRequestLog = 20
	// maxRequestAttempts bounds refresh-requests per rejected access token.
	maxRequestAttempts = 5
	// requestRetryMin is the minimum spacing between repeated requests.
	requestRetryMin = 60 * time.Second
)

// tokenTiming is when the access token currently in a mount was issued and how
// long it lives.
type tokenTiming struct {
	AccessHash string
	ObtainedAt int64
	ExpiresIn  int64
}

type pendingRequest struct {
	groupID    string
	mount      string
	accessHash string
	deadline   time.Time
	attempts   int
	lastPull   time.Time
}

// RequestView is one refresh-request, as sent or received by this node.
type RequestView struct {
	Time      int64  `json:"time"`
	Direction string `json:"direction"` // "sent" | "received"
	Peer      string `json:"peer"`
	PeerLabel string `json:"peer_label"`
	GroupID   string `json:"group_id"`
	Mount     string `json:"mount"`
	Result    string `json:"result"`
}

// refresherState is the Manager's refresher bookkeeping. The zero value is
// usable: maps are created on first use under mu.
type refresherState struct {
	mu           sync.Mutex
	startedAt    time.Time
	acquiredAt   map[string]time.Time // group -> when this process claimed the lease
	renewedAt    map[string]time.Time // group -> last time we issued our lease
	taking       map[string]bool
	noLeaseSince map[string]time.Time
	lastRefresh  map[string]*refreshSummary // group -> holder-side summary
	timing       map[string]tokenTiming     // mount -> access token timing
	refreshing   map[string]bool            // mount -> refresh in flight
	lastAttempt  map[string]time.Time       // mount -> last refresh attempt start
	failStreak   map[string]int             // mount -> consecutive failed attempts
	pending      map[string]*pendingRequest // group\0mount
	requests     []RequestView
	warnedLegacy map[string]time.Time
	loopStarted  bool
}

func (r *refresherState) ensureLocked() {
	if r.acquiredAt != nil {
		return
	}
	r.acquiredAt = map[string]time.Time{}
	r.renewedAt = map[string]time.Time{}
	r.taking = map[string]bool{}
	r.noLeaseSince = map[string]time.Time{}
	r.lastRefresh = map[string]*refreshSummary{}
	r.timing = map[string]tokenTiming{}
	r.refreshing = map[string]bool{}
	r.lastAttempt = map[string]time.Time{}
	r.failStreak = map[string]int{}
	r.pending = map[string]*pendingRequest{}
	r.warnedLegacy = map[string]time.Time{}
}

// ---- roles ----

func (m *Manager) isHolder(groupID string) bool {
	e := m.state.leaseOf(groupID)
	return e != nil && e.lease.Holder == m.id.NodeID && e.valid(time.Now())
}

// holderOf returns the node id of the valid lease holder, if any.
func (m *Manager) holderOf(groupID string) (string, bool) {
	e := m.state.leaseOf(groupID)
	if e == nil || !e.valid(time.Now()) {
		return "", false
	}
	return e.lease.Holder, true
}

// settled reports whether our claim on the lease is old enough to refresh.
// A lease inherited from a previous run (never claimed by this process) has no
// settle period.
func (m *Manager) settled(groupID string) bool {
	m.refr.mu.Lock()
	defer m.refr.mu.Unlock()
	m.refr.ensureLocked()
	at, ok := m.refr.acquiredAt[groupID]
	return !ok || time.Since(at) >= settleDelay
}

// isRefreshGroup reports whether a group takes part in refresher leases: at
// least one member mount runs the driver with single-use refresh tokens.
func (m *Manager) isRefreshGroup(g group) bool {
	byNode := map[string]nodeInfo{}
	for _, n := range m.state.inventoryList() {
		byNode[n.NodeID] = n
	}
	for _, mem := range g.Members {
		if mem.NodeID == m.id.NodeID {
			if d, err := op.GetStorageByMountPath(mem.MountPath); err == nil {
				if _, ok := d.(op.TokenRefresher); ok {
					return true
				}
			}
			continue
		}
		for _, st := range byNode[mem.NodeID].Storages {
			if st.MountPath == mem.MountPath && st.Driver == refreshDriverName {
				return true
			}
		}
	}
	return false
}

// MayRefresh implements op.RefreshCoordinator: only the group's lease holder may
// spend the refresh_token. Mounts outside any group, nodes that do not adopt
// remote credentials, and a disabled cluster keep the plain self-refresh
// behaviour.
func (m *Manager) MayRefresh(mount string) bool {
	if m.cfgStore == nil {
		return true
	}
	cfg := m.cfgStore.get()
	if !cfg.active() || !cfg.ApplyRemote {
		return true
	}
	groups := m.state.groupsForMount(m.id.NodeID, mount)
	strict := false
	for _, g := range groups {
		if !m.isRefreshGroup(g) {
			continue
		}
		strict = true
		if m.isHolder(g.ID) && m.settled(g.ID) {
			return true
		}
	}
	return !strict
}

// ---- takeover order ----

// refreshCandidate is one member node considered for the lease.
type refreshCandidate struct {
	NodeID    string
	Public    bool // advertises a dialable address
	Working   bool // has a mount in the group currently WORK
	Preferred bool
}

// refreshCandidates lists the group's member nodes that are online and have a
// usable mount. Self always counts as online.
func (m *Manager) refreshCandidates(g group) []refreshCandidate {
	byNode := map[string]nodeInfo{}
	for _, n := range m.state.inventoryList() {
		byNode[n.NodeID] = n
	}
	pref := ""
	if p := m.state.policyOf(g.ID); p != nil {
		pref = p.Preferred
	}
	seen := map[string]bool{}
	var out []refreshCandidate
	for _, mem := range g.Members {
		if seen[mem.NodeID] {
			continue
		}
		seen[mem.NodeID] = true
		c := refreshCandidate{NodeID: mem.NodeID, Preferred: mem.NodeID == pref}
		if mem.NodeID == m.id.NodeID {
			c.Public = trimURL(m.cfgStore.get().Addr) != ""
			present := false
			for _, mp := range g.mountsForNode(mem.NodeID) {
				d, err := op.GetStorageByMountPath(mp)
				if err != nil {
					continue
				}
				present = true
				if d.GetStorage().Status == op.WORK {
					c.Working = true
				}
			}
			if !present {
				continue
			}
		} else {
			n, ok := byNode[mem.NodeID]
			if !ok || !m.isOnline(mem.NodeID, n.seenAt) {
				continue
			}
			c.Public = n.Addr != ""
			present := false
			for _, mp := range g.mountsForNode(mem.NodeID) {
				for _, st := range n.Storages {
					if st.MountPath == mp {
						present = true
						if st.Status == op.WORK {
							c.Working = true
						}
					}
				}
			}
			if !present {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// takeoverOrder ranks candidates: the admin's preferred node first, then nodes
// with a working mount, then nodes with a public address, then by node id. The
// ranking depends only on shared state, so every node computes the same order.
func takeoverOrder(cands []refreshCandidate) []string {
	cp := make([]refreshCandidate, len(cands))
	copy(cp, cands)
	sort.SliceStable(cp, func(i, j int) bool {
		a, b := cp[i], cp[j]
		if a.Preferred != b.Preferred {
			return a.Preferred
		}
		if a.Working != b.Working {
			return a.Working
		}
		if a.Public != b.Public {
			return a.Public
		}
		return a.NodeID < b.NodeID
	})
	out := make([]string, len(cp))
	for i, c := range cp {
		out[i] = c.NodeID
	}
	return out
}

// ---- loop ----

// leaseTick is the evaluation period of the refresher loop.
func (m *Manager) leaseTick() time.Duration {
	t := m.cfgStore.get().leaseTTL() / 6
	if t < 500*time.Millisecond {
		t = 500 * time.Millisecond
	}
	if t > 10*time.Second {
		t = 10 * time.Second
	}
	return t
}

func (m *Manager) refresherLoop() {
	m.refr.mu.Lock()
	m.refr.ensureLocked()
	m.refr.startedAt = time.Now()
	m.refr.mu.Unlock()
	for {
		select {
		case <-m.stopCh:
			return
		case <-time.After(m.leaseTick()):
		}
		m.refresherTick()
	}
}

func (m *Manager) refresherTick() {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		return
	}
	if cfg.ApplyRemote {
		for _, g := range m.state.groupList() {
			if len(g.mountsForNode(m.id.NodeID)) == 0 || !m.isRefreshGroup(g) {
				continue
			}
			m.leaseStep(g)
			m.proactiveStep(g)
		}
		m.requestTick()
	}
	m.deliveryTick()
	if n := m.state.pruneRevocations(); n > 0 {
		utils.Log.Infof("[cluster] pruned %d superseded revocation(s) past retention", n)
		_ = m.persist()
	}
}

// leaseStep keeps the lease alive when held and claims it when it is vacant.
func (m *Manager) leaseStep(g group) {
	cfg := m.cfgStore.get()
	ttl := cfg.leaseTTL()
	self := m.id.NodeID
	e := m.state.leaseOf(g.ID)
	valid := e != nil && e.valid(time.Now())

	m.refr.mu.Lock()
	m.refr.ensureLocked()
	if valid {
		delete(m.refr.noLeaseSince, g.ID)
	} else if _, ok := m.refr.noLeaseSince[g.ID]; !ok {
		m.refr.noLeaseSince[g.ID] = time.Now()
	}
	noLeaseFor := time.Since(m.refr.noLeaseSince[g.ID])
	started := m.refr.startedAt
	taking := m.refr.taking[g.ID]
	renewed := m.refr.renewedAt[g.ID]
	acquired := m.refr.acquiredAt[g.ID]
	m.refr.mu.Unlock()

	cands := m.refreshCandidates(g)
	order := takeoverOrder(cands)

	if valid && e.lease.Holder == self {
		healthy := false
		for _, c := range cands {
			if c.NodeID == self && c.Working {
				healthy = true
			}
		}
		if !healthy && !m.hasRecoverableMount(g) {
			// Nobody can refresh through a mount that is not working; let a
			// healthy member take over rather than hold the role idle.
			m.releaseLease(g, "no working mount")
			return
		}
		if pref := m.preferredOnline(g, cands); pref != "" && pref != self && time.Since(acquiredOr(acquired, started)) >= ttl {
			m.releaseLease(g, "handing over to preferred refresher "+shortNode(pref))
			return
		}
		if time.Since(renewed) >= ttl/3 {
			m.renewLease(g)
		}
		return
	}
	if valid {
		// A holder that left the group or never shows up as a member can't be
		// renewing: its lease simply runs out and is claimed below.
		return
	}
	if taking || noLeaseFor < 0 {
		return
	}
	if time.Since(started) < m.startupGrace(ttl) {
		return
	}
	rank := -1
	for i, id := range order {
		if id == self {
			rank = i
		}
	}
	if rank < 0 {
		return
	}
	stagger := ttl / 3
	if stagger < time.Second {
		stagger = time.Second
	}
	if noLeaseFor < time.Duration(rank)*stagger {
		return
	}
	m.refr.mu.Lock()
	m.refr.taking[g.ID] = true
	m.refr.mu.Unlock()
	go m.takeover(g, e)
}

func acquiredOr(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}

func (m *Manager) startupGrace(ttl time.Duration) time.Duration {
	g := ttl / 2
	if g > 15*time.Second {
		g = 15 * time.Second
	}
	return g
}

// preferredOnline returns the preferred node when it is online with a working
// mount, "" otherwise.
func (m *Manager) preferredOnline(g group, cands []refreshCandidate) string {
	for _, c := range cands {
		if c.Preferred && c.Working {
			return c.NodeID
		}
	}
	return ""
}

// hasRecoverableMount is true when one of our group mounts failed only because
// its access token expired, which the refresher itself can repair.
func (m *Manager) hasRecoverableMount(g group) bool {
	for _, mp := range g.mountsForNode(m.id.NodeID) {
		d, err := op.GetStorageByMountPath(mp)
		if err != nil {
			continue
		}
		if looksLikeAccessExpired(d.GetStorage().Status) {
			return true
		}
	}
	return false
}

// looksLikeAccessExpired recognises a 401xxxxx provider error in a storage
// status: a mount that failed to initialise on an expired access token.
func looksLikeAccessExpired(status string) bool {
	return strings.Contains(status, "code: 401")
}

// takeover claims a vacant lease. It first pulls the group's latest credential
// from every connected peer, so the new holder never spends a stale
// refresh_token, and re-checks that nobody claimed the lease meanwhile.
func (m *Manager) takeover(g group, prev *leaseEntry) {
	defer func() {
		m.refr.mu.Lock()
		delete(m.refr.taking, g.ID)
		m.refr.mu.Unlock()
	}()
	prevHolder := ""
	var prevEpoch uint64
	if prev != nil {
		prevHolder, prevEpoch = prev.lease.Holder, prev.lease.Epoch
	}
	utils.Log.Infof("[cluster] lease takeover begin group=%s prev_holder=%s prev_epoch=%d: pulling latest credential first",
		g.ID, shortNode(prevHolder), prevEpoch)
	start := time.Now()
	m.pullGroup(g.ID)
	m.waitPullReplies(start, m.pullWait())

	if cur := m.state.leaseOf(g.ID); cur != nil && cur.valid(time.Now()) && cur.lease.Holder != m.id.NodeID {
		utils.Log.Infof("[cluster] lease takeover aborted group=%s: %s claimed it first", g.ID, shortNode(cur.lease.Holder))
		return
	}
	epoch := prevEpoch
	if cur := m.state.leaseOf(g.ID); cur != nil && cur.lease.Epoch > epoch {
		epoch = cur.lease.Epoch
	}
	epoch++
	l := m.state.issueLease(m.id, g.ID, epoch, m.nextSeq(g.ID), int64(m.cfgStore.get().leaseTTL()/time.Second), m.lastSummary(g.ID, prev), time.Now())
	m.refr.mu.Lock()
	m.refr.ensureLocked()
	m.refr.acquiredAt[g.ID] = time.Now()
	m.refr.renewedAt[g.ID] = time.Now()
	m.refr.mu.Unlock()
	if err := m.persist(); err != nil {
		utils.Log.Warnf("[cluster] persist lease: %v", err)
	}
	m.broadcast(&syncMessage{Type: "push", Leases: []*refresherLease{l}})
	m.recordEvent("lease", g.ID, fmt.Sprintf("took over the refresher role (epoch %d, previous %s)", epoch, shortNode(prevHolder)))
	utils.Log.Infof("[cluster] lease acquired group=%s epoch=%d prev_holder=%s ttl=%ds", g.ID, epoch, shortNode(prevHolder), l.TTLSec)
}

func (m *Manager) pullWait() time.Duration {
	w := m.cfgStore.get().leaseTTL() / 4
	if w > 3*time.Second {
		w = 3 * time.Second
	}
	if w < 300*time.Millisecond {
		w = 300 * time.Millisecond
	}
	return w
}

// waitPullReplies waits until every live link has delivered a frame since start,
// or the timeout passes. Replies to our pull arrive on those links.
func (m *Manager) waitPullReplies(start time.Time, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		all := true
		for _, c := range m.conns.all() {
			if c.lastRecv.Load() < start.UnixNano() {
				all = false
			}
		}
		if all {
			return
		}
		select {
		case <-m.stopCh:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (m *Manager) nextSeq(groupID string) uint64 {
	seq := uint64(time.Now().Unix())
	if e := m.state.leaseOf(groupID); e != nil && e.lease.Seq >= seq {
		seq = e.lease.Seq + 1
	}
	return seq
}

func (m *Manager) lastSummary(groupID string, prev *leaseEntry) *refreshSummary {
	m.refr.mu.Lock()
	defer m.refr.mu.Unlock()
	m.refr.ensureLocked()
	if s := m.refr.lastRefresh[groupID]; s != nil {
		cp := *s
		return &cp
	}
	if prev != nil && prev.lease.Last != nil {
		cp := *prev.lease.Last
		return &cp
	}
	return nil
}

// renewLease re-issues our lease (growing Seq, refreshing the last-refresh summary).
func (m *Manager) renewLease(g group) {
	e := m.state.leaseOf(g.ID)
	if e == nil || e.lease.Holder != m.id.NodeID {
		return
	}
	l := m.state.issueLease(m.id, g.ID, e.lease.Epoch, m.nextSeq(g.ID), int64(m.cfgStore.get().leaseTTL()/time.Second), m.lastSummary(g.ID, e), time.Now())
	m.refr.mu.Lock()
	m.refr.ensureLocked()
	m.refr.renewedAt[g.ID] = time.Now()
	m.refr.mu.Unlock()
	m.broadcast(&syncMessage{Type: "push", Leases: []*refresherLease{l}})
}

// releaseLease gives the role up (TTL 0): the next member in order claims it.
func (m *Manager) releaseLease(g group, why string) {
	e := m.state.leaseOf(g.ID)
	if e == nil || e.lease.Holder != m.id.NodeID {
		return
	}
	l := m.state.issueLease(m.id, g.ID, e.lease.Epoch, m.nextSeq(g.ID), 0, m.lastSummary(g.ID, e), time.Now())
	m.refr.mu.Lock()
	m.refr.ensureLocked()
	delete(m.refr.acquiredAt, g.ID)
	m.refr.mu.Unlock()
	_ = m.persist()
	m.broadcast(&syncMessage{Type: "push", Leases: []*refresherLease{l}})
	m.recordEvent("lease", g.ID, "released the refresher role: "+why)
	utils.Log.Infof("[cluster] lease released group=%s epoch=%d: %s", g.ID, e.lease.Epoch, why)
}

// absorbLeases merges verified leases; it returns those that replaced ours.
func (m *Manager) absorbLeases(leases []*refresherLease) []*refresherLease {
	var merged []*refresherLease
	changedHolder := false
	groups := m.state.groupList()
	for _, l := range leases {
		if !leaseAuthorizedByGroups(groups, l) {
			continue
		}
		prev := m.state.leaseOf(l.GroupID)
		if !m.state.mergeLease(l, time.Now()) {
			continue
		}
		merged = append(merged, l)
		if prev == nil || prev.lease.Holder != l.Holder || prev.lease.Epoch != l.Epoch || (prev.lease.TTLSec == 0) != (l.TTLSec == 0) {
			changedHolder = true
			if l.TTLSec > 0 {
				utils.Log.Infof("[cluster] lease group=%s holder=%s epoch=%d (was %s)", l.GroupID, shortNode(l.Holder), l.Epoch, holderName(prev))
			} else {
				utils.Log.Infof("[cluster] lease group=%s released by %s epoch=%d", l.GroupID, shortNode(l.Holder), l.Epoch)
			}
			if prev != nil && prev.lease.Holder == m.id.NodeID && l.Holder != m.id.NodeID {
				m.refr.mu.Lock()
				m.refr.ensureLocked()
				delete(m.refr.acquiredAt, l.GroupID)
				m.refr.mu.Unlock()
				m.recordEvent("lease", l.GroupID, "stood down: "+shortNode(l.Holder)+" holds a newer lease")
			}
		}
	}
	if changedHolder {
		_ = m.persist()
	}
	return merged
}

func holderName(e *leaseEntry) string {
	if e == nil {
		return "none"
	}
	return shortNode(e.lease.Holder)
}

func (m *Manager) absorbPolicies(ps []*groupPolicy) []*groupPolicy {
	var merged []*groupPolicy
	for _, p := range ps {
		if !p.verify() {
			continue
		}
		if _, ok := m.state.groupByID(p.GroupID); !ok {
			continue
		}
		if m.state.mergePolicy(p) {
			merged = append(merged, p)
			utils.Log.Infof("[cluster] preferred refresher for group=%s set to %q by %s (v%d)", p.GroupID, shortNode(p.Preferred), shortNode(p.Origin), p.Version)
		}
	}
	if len(merged) > 0 {
		_ = m.persist()
	}
	return merged
}

// SetPreferredRefresher records the admin's preferred refresher for a group
// (node id, "" to clear) and replicates it.
func (m *Manager) SetPreferredRefresher(groupID, nodeID string) error {
	g, ok := m.state.groupByID(groupID)
	if !ok {
		return fmt.Errorf("unknown group %q", groupID)
	}
	if nodeID != "" {
		member := false
		for _, mem := range g.Members {
			if mem.NodeID == nodeID {
				member = true
			}
		}
		if !member {
			return fmt.Errorf("node %s is not a member of group %s", nodeID, groupID)
		}
	}
	p := m.state.setPolicy(m.id, groupID, nodeID, now())
	if err := m.persist(); err != nil {
		return fmt.Errorf("persist policy: %w", err)
	}
	m.recordEvent("lease", groupID, fmt.Sprintf("preferred refresher set to %s", shortNode(nodeID)))
	utils.Log.Infof("[cluster] preferred refresher for group=%s set to %q (v%d)", groupID, shortNode(nodeID), p.Version)
	m.broadcast(&syncMessage{Type: "push", Policies: []*groupPolicy{p}})
	return nil
}

// ---- proactive refresh ----

func accessTokenOf(additionJSON string) string {
	var all map[string]json.RawMessage
	if json.Unmarshal([]byte(additionJSON), &all) != nil {
		return ""
	}
	var s string
	if raw, ok := all["access_token"]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// timingFor returns when the access token in the mount was obtained and its
// lifetime, from the local refresh record or from the catalogued record of the
// pair (adopted from a peer). ok is false when unknown.
func (m *Manager) timingFor(mount, additionJSON string) (obtained, expiresIn int64, ok bool) {
	hash := op.TokenHash(accessTokenOf(additionJSON))
	m.refr.mu.Lock()
	m.refr.ensureLocked()
	t, have := m.refr.timing[mount]
	m.refr.mu.Unlock()
	if have && hash != "" && t.AccessHash == hash && t.ExpiresIn > 0 {
		return t.ObtainedAt, t.ExpiresIn, true
	}
	h := credHash(extractCreds(additionJSON))
	for _, g := range m.state.groupsForMount(m.id.NodeID, mount) {
		if rec, found := m.state.credByHash(g.ID, h); found && rec.ExpiresIn > 0 && rec.ObtainedAt > 0 {
			return rec.ObtainedAt, rec.ExpiresIn, true
		}
	}
	return 0, 0, false
}

// refreshDue decides whether the holder should refresh a mount now. Unknown
// expiry never triggers a refresh: the holder waits for an expiry signal.
func (m *Manager) refreshDue(mount, status, additionJSON string) (trigger string, due bool) {
	if status == op.WORK {
		obtained, exp, ok := m.timingFor(mount, additionJSON)
		if !ok {
			return "", false
		}
		at := obtained + int64(float64(exp)*proactiveFraction)
		if now() >= at {
			return "proactive", true
		}
		return "", false
	}
	if looksLikeAccessExpired(status) {
		return "recover", true
	}
	return "", false
}

func (m *Manager) proactiveStep(g group) {
	if !m.isHolder(g.ID) || !m.settled(g.ID) {
		return
	}
	for _, mp := range g.mountsForNode(m.id.NodeID) {
		d, err := op.GetStorageByMountPath(mp)
		if err != nil {
			continue
		}
		if _, ok := d.(op.TokenRefresher); !ok {
			continue
		}
		st := d.GetStorage()
		trigger, due := m.refreshDue(mp, st.Status, st.Addition)
		if !due {
			continue
		}
		m.startRefresh(mp, trigger)
	}
}

// refreshGap is the minimum spacing between refresh attempts of one mount:
// the configured gap, doubling per consecutive failure up to 10 minutes. This
// bounds proactive refresh to a handful of calls per hour even if the provider
// reports odd lifetimes or keeps failing.
func (m *Manager) refreshGap(mount string) time.Duration {
	gap := m.cfgStore.get().proactiveMinGap()
	m.refr.mu.Lock()
	n := m.refr.failStreak[mount]
	m.refr.mu.Unlock()
	for i := 0; i < n && gap < 10*time.Minute; i++ {
		gap *= 2
	}
	if gap > 10*time.Minute {
		gap = 10 * time.Minute
	}
	return gap
}

// startRefresh runs one refresh of a mount in the background. It returns false
// when one is already running or the attempt is rate limited.
func (m *Manager) startRefresh(mount, trigger string) bool {
	gap := m.refreshGap(mount)
	m.refr.mu.Lock()
	m.refr.ensureLocked()
	if m.refr.refreshing[mount] || time.Since(m.refr.lastAttempt[mount]) < gap {
		m.refr.mu.Unlock()
		return false
	}
	m.refr.refreshing[mount] = true
	m.refr.lastAttempt[mount] = time.Now()
	m.refr.mu.Unlock()
	go func() {
		defer func() {
			m.refr.mu.Lock()
			delete(m.refr.refreshing, mount)
			m.refr.mu.Unlock()
		}()
		d, err := op.GetStorageByMountPath(mount)
		if err != nil {
			return
		}
		tr, ok := d.(op.TokenRefresher)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		utils.Log.Infof("[cluster] refresher refreshing mount=%s trigger=%s", mount, trigger)
		if err := tr.RefreshTokenNow(ctx, trigger); err != nil {
			m.refr.mu.Lock()
			m.refr.failStreak[mount]++
			m.refr.mu.Unlock()
			utils.Log.Warnf("[cluster] refresher refresh mount=%s trigger=%s failed: %v", mount, trigger, err)
			return
		}
		m.refr.mu.Lock()
		m.refr.failStreak[mount] = 0
		m.refr.mu.Unlock()
	}()
	return true
}

// ---- op.RefreshCoordinator: observation ----

// ReportRefresh implements op.RefreshCoordinator. A real attempt updates the
// token timing, the holder's last-refresh summary and the activity log; skips
// are only counted in the driver's log.
func (m *Manager) ReportRefresh(r op.RefreshReport) {
	switch r.Outcome {
	case "ok", "api_error", "transport_error":
	default:
		return
	}
	m.refr.mu.Lock()
	m.refr.ensureLocked()
	if r.Outcome == "ok" && r.AccessTokenHash != "" {
		m.refr.timing[r.Mount] = tokenTiming{AccessHash: r.AccessTokenHash, ObtainedAt: r.Time.Unix(), ExpiresIn: r.ExpiresIn}
	}
	m.refr.mu.Unlock()
	if m.state == nil || m.id == nil {
		return
	}
	sum := &refreshSummary{Time: r.Time.Unix(), Mount: r.Mount, Trigger: r.Trigger, Outcome: r.Outcome, ErrCode: r.ErrCode, ExpiresIn: r.ExpiresIn}
	for _, g := range m.state.groupsForMount(m.id.NodeID, r.Mount) {
		m.refr.mu.Lock()
		m.refr.lastRefresh[g.ID] = sum
		m.refr.mu.Unlock()
		detail := fmt.Sprintf("%s refresh %s (trigger %s", r.Mount, r.Outcome, r.Trigger)
		if r.ErrCode != 0 {
			detail += fmt.Sprintf(", code %d", r.ErrCode)
		}
		if r.ExpiresIn > 0 {
			detail += fmt.Sprintf(", expires_in %ds", r.ExpiresIn)
		}
		m.recordEvent("refresh", g.ID, detail+")")
		if m.isHolder(g.ID) {
			go m.renewLease(g) // carries the summary to every node
		}
	}
}

// RefreshRejected implements op.RefreshCoordinator: a member that may not refresh
// had its access token rejected. It pulls from peers now and, if no newer pair
// arrives within the wait, asks the refresher. Repeated rejections of the same
// token (a player retrying) only re-pull every few seconds.
func (m *Manager) RefreshRejected(mount, accessHash string) {
	if m.cfgStore == nil || !m.cfgStore.get().active() {
		return
	}
	wait := m.cfgStore.get().refreshRequestWait()
	for _, g := range m.state.groupsForMount(m.id.NodeID, mount) {
		key := g.ID + "\x00" + mount
		pull := false
		m.refr.mu.Lock()
		m.refr.ensureLocked()
		p := m.refr.pending[key]
		switch {
		case p == nil || p.accessHash != accessHash:
			m.refr.pending[key] = &pendingRequest{groupID: g.ID, mount: mount, accessHash: accessHash, deadline: time.Now().Add(wait), lastPull: time.Now()}
			utils.Log.Infof("[cluster] token rejected on %s (group %s): pulling; will ask the refresher if no newer version in %s", mount, g.ID, wait)
			pull = true
		case time.Since(p.lastPull) > 10*time.Second:
			p.lastPull = time.Now()
			pull = true
		}
		m.refr.mu.Unlock()
		if pull {
			m.pullGroup(g.ID)
		}
	}
}

// requestTick sends due refresh-requests and forgets those answered by a new pair.
func (m *Manager) requestTick() {
	m.refr.mu.Lock()
	m.refr.ensureLocked()
	var due []*pendingRequest
	for key, p := range m.refr.pending {
		d, err := op.GetStorageByMountPath(p.mount)
		if err != nil || op.TokenHash(accessTokenOf(d.GetStorage().Addition)) != p.accessHash {
			delete(m.refr.pending, key) // a newer pair arrived (or the mount is gone)
			continue
		}
		if time.Now().Before(p.deadline) {
			continue
		}
		cp := *p
		due = append(due, &cp)
		p.attempts++
		p.deadline = time.Now().Add(maxDuration(requestRetryMin, m.cfgStore.get().refreshRequestWait()*2))
		if p.attempts >= maxRequestAttempts {
			delete(m.refr.pending, key)
		}
	}
	m.refr.mu.Unlock()
	for _, p := range due {
		holder, ok := m.holderOf(p.groupID)
		if !ok {
			m.logRequest("sent", "", p.groupID, p.mount, "no live refresher; waiting for takeover")
			continue
		}
		if holder == m.id.NodeID {
			continue
		}
		var haveVer uint64
		if st, err := op.GetStorageByMountPath(p.mount); err == nil {
			if rec, found := m.state.credByHash(p.groupID, credHash(extractCreds(st.GetStorage().Addition))); found {
				haveVer = rec.Version
			}
		}
		h := credHash(extractCreds(m.mountAddition(p.mount)))
		m.sendDirected(holder, &syncMessage{Type: msgRefreshRequest, Req: &refreshRequest{
			GroupID: p.groupID, Mount: p.mount, HaveHash: shortHash(h), HaveVersion: haveVer,
		}})
		m.logRequest("sent", holder, p.groupID, p.mount, "sent")
		utils.Log.Infof("[cluster] refresh-request sent to %s group=%s mount=%s have_v=%d", shortNode(holder), p.groupID, p.mount, haveVer)
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func (m *Manager) mountAddition(mount string) string {
	d, err := op.GetStorageByMountPath(mount)
	if err != nil {
		return ""
	}
	return d.GetStorage().Addition
}

func (m *Manager) logRequest(dir, peer, groupID, mount, result string) {
	m.refr.mu.Lock()
	m.refr.ensureLocked()
	m.refr.requests = append(m.refr.requests, RequestView{
		Time: now(), Direction: dir, Peer: peer, PeerLabel: m.labelOf(peer), GroupID: groupID, Mount: mount, Result: result,
	})
	if len(m.refr.requests) > maxRequestLog {
		m.refr.requests = m.refr.requests[len(m.refr.requests)-maxRequestLog:]
	}
	m.refr.mu.Unlock()
}

func (m *Manager) requestLog() []RequestView {
	m.refr.mu.Lock()
	defer m.refr.mu.Unlock()
	out := make([]RequestView, len(m.refr.requests))
	copy(out, m.refr.requests)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (m *Manager) labelOf(nodeID string) string {
	if nodeID == "" {
		return ""
	}
	for _, n := range m.state.inventoryList() {
		if n.NodeID == nodeID && n.Label != "" {
			return n.Label
		}
	}
	return shortNode(nodeID)
}

// handleRefreshRequest runs on the lease holder when a member reports a rejected
// token. A requester that is behind just gets our current pair re-sent; one that
// already has our newest pair gets a real refresh, rate limited per mount.
func (m *Manager) handleRefreshRequest(from string, req *refreshRequest) {
	if req == nil {
		return
	}
	g, ok := m.state.groupByID(req.GroupID)
	if !ok {
		return
	}
	member := false
	for _, mem := range g.Members {
		if mem.NodeID == from {
			member = true
		}
	}
	if !member {
		m.logRequest("received", from, req.GroupID, req.Mount, "ignored: not a member")
		return
	}
	if !m.isHolder(req.GroupID) {
		m.logRequest("received", from, req.GroupID, req.Mount, "ignored: not the refresher")
		return
	}
	cur, hasCur := m.state.getCred(req.GroupID)
	if hasCur && cur.Version > req.HaveVersion && shortHash(cur.CredHash) != req.HaveHash {
		m.sendDirected(from, &syncMessage{Type: "push", Creds: m.state.credsForGroup(req.GroupID)})
		m.logRequest("received", from, req.GroupID, req.Mount, "requester behind: resent current pair")
		utils.Log.Infof("[cluster] refresh-request from %s group=%s: requester behind (have v%d, ours v%d), resent", shortNode(from), req.GroupID, req.HaveVersion, cur.Version)
		return
	}
	started := false
	for _, mp := range g.mountsForNode(m.id.NodeID) {
		if d, err := op.GetStorageByMountPath(mp); err == nil {
			if _, ok := d.(op.TokenRefresher); ok {
				if m.startRefresh(mp, "refresh-request") {
					started = true
				}
			}
		}
	}
	res := "refresh started"
	if !started {
		res = "rate limited or already refreshing"
		if hasCur {
			m.sendDirected(from, &syncMessage{Type: "push", Creds: m.state.credsForGroup(req.GroupID)})
			res += "; resent current pair"
		}
	}
	m.logRequest("received", from, req.GroupID, req.Mount, res)
	utils.Log.Infof("[cluster] refresh-request from %s group=%s mount=%s: %s", shortNode(from), req.GroupID, req.Mount, res)
}

// sendDirected addresses a message to one node: over its direct link when there
// is one, otherwise to every link so a public hub can forward it.
func (m *Manager) sendDirected(to string, msg *syncMessage) {
	if m.conns == nil {
		return
	}
	msg.From = m.id.NodeID
	msg.To = to
	msg.Proto = protoLevel
	msg.Hops = 0
	if c := m.conns.get(to); c != nil {
		c.enqueue(msg)
		return
	}
	for _, c := range m.conns.all() {
		c.enqueue(msg)
	}
}

// forwardDirected relays a directed message that is not for us.
func (m *Manager) forwardDirected(msg *syncMessage, from *peerConn) {
	const maxHops = 3
	if msg.Hops >= maxHops {
		return
	}
	cp := *msg
	cp.Hops++
	if c := m.conns.get(msg.To); c != nil && c != from {
		c.enqueue(&cp)
		return
	}
	for _, c := range m.conns.all() {
		if c != from {
			c.enqueue(&cp)
		}
	}
}

// heldCred reports the credential pair a mount is using: its short hash and the
// catalogue version of that pair (0 when the mount is in no group or the pair is
// not catalogued, e.g. a pair the admin typed in).
func (m *Manager) heldCred(st model.Storage) (hash string, ver uint64) {
	if m.state == nil || m.id == nil {
		return "", 0
	}
	groups := m.state.groupsForMount(m.id.NodeID, st.MountPath)
	if len(groups) == 0 {
		return "", 0
	}
	h := credHash(extractCreds(st.Addition))
	if h == "" {
		return "", 0
	}
	for _, g := range groups {
		if rec, ok := m.state.credByHash(g.ID, h); ok {
			return shortHash(h), rec.Version
		}
	}
	return shortHash(h), 0
}

// timingForPayload returns the lifetime of the access token inside a credential
// payload when this node refreshed it (matched by token hash).
func (m *Manager) timingForPayload(mount string, creds map[string]json.RawMessage) (obtained, expiresIn int64) {
	var tok string
	if raw, ok := creds["access_token"]; ok {
		_ = json.Unmarshal(raw, &tok)
	}
	hash := op.TokenHash(tok)
	m.refr.mu.Lock()
	defer m.refr.mu.Unlock()
	m.refr.ensureLocked()
	if t, ok := m.refr.timing[mount]; ok && hash != "" && t.AccessHash == hash {
		return t.ObtainedAt, t.ExpiresIn
	}
	return 0, 0
}

// linkCount is the number of live links (for log lines).
func (m *Manager) linkCount() int {
	if m.conns == nil {
		return 0
	}
	return len(m.conns.all())
}
