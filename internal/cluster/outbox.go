package cluster

import (
	"sort"
	"sync"
)

// outbox is the per-peer send state. It replaces a FIFO of sealed frames: every
// kind of state keeps only its latest pending value, so a slow link can never
// overflow it, a stalled writer never costs a connection, and nothing is ever
// dropped that a newer value would not have superseded anyway.
//
//	priority class (flushed first): credentials, revocations, refresher leases
//	  and policies, directed control messages (refresh-request, ack, ...)
//	bulk class: the latest hello/announce snapshot, a pull, and the latest
//	  group document / inventory entries / reply wants
//
// Memory is bounded by the cluster's own cardinality: (group, origin, mount)
// credentials, (group, pair) revocations, one lease and policy per group, one
// inventory entry per node, and a capped list of directed messages.
type outbox struct {
	mu   sync.Mutex
	wake chan struct{}

	creds    map[string]*credRecord
	revs     map[string]*credRevocation
	leases   map[string]*refresherLease
	policies map[string]*groupPolicy
	directed []*syncMessage

	announce   *syncMessage // latest hello/announce
	groups     *groupDoc
	nodes      map[string]*nodeInfo
	pullWants  map[string]struct{}
	wantRevs   bool
	replyWants map[string]struct{}
}

// maxDirected bounds the queue of directed control messages; the oldest is
// dropped first (requests and acks are retried by their owners).
const maxDirected = 64

func newOutbox() *outbox {
	return &outbox{
		wake:       make(chan struct{}, 1),
		creds:      make(map[string]*credRecord),
		revs:       make(map[string]*credRevocation),
		leases:     make(map[string]*refresherLease),
		policies:   make(map[string]*groupPolicy),
		nodes:      make(map[string]*nodeInfo),
		pullWants:  make(map[string]struct{}),
		replyWants: make(map[string]struct{}),
	}
}

func revKey(r *credRevocation) string { return r.GroupID + "\x00" + r.CredHash }

// put coalesces msg into the pending state. It only reads msg.
func (o *outbox) put(msg *syncMessage) {
	if msg == nil {
		return
	}
	o.mu.Lock()
	o.putLocked(msg)
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *outbox) putLocked(msg *syncMessage) {
	if msg.To != "" || msg.Type == msgRefreshRequest || msg.Type == msgAck {
		o.directed = append(o.directed, msg)
		if len(o.directed) > maxDirected {
			o.directed = o.directed[len(o.directed)-maxDirected:]
		}
		return
	}
	for _, r := range msg.Creds {
		if r == nil {
			continue
		}
		k := credDigestKey(digestOfCred(r))
		if cur := o.creds[k]; cur == nil || r.dominates(cur) {
			o.creds[k] = r
		}
	}
	for _, r := range msg.Revocations {
		if r == nil {
			continue
		}
		k := revKey(r)
		if cur := o.revs[k]; cur == nil || r.dominates(cur) {
			o.revs[k] = r
		}
	}
	for _, l := range msg.Leases {
		if l == nil {
			continue
		}
		if cur := o.leases[l.GroupID]; cur == nil || l.dominates(cur) {
			o.leases[l.GroupID] = l
		}
	}
	for _, p := range msg.Policies {
		if p == nil {
			continue
		}
		if cur := o.policies[p.GroupID]; cur == nil || p.dominates(cur) {
			o.policies[p.GroupID] = p
		}
	}
	switch msg.Type {
	case "hello", "announce":
		// A full snapshot supersedes any earlier one. Keep "hello" semantics
		// out of it: both trigger the same anti-entropy reply on the peer.
		o.announce = msg
	case "pull":
		for _, g := range msg.Wants {
			o.pullWants[g] = struct{}{}
		}
		if msg.WantRevs {
			o.wantRevs = true
		}
	default: // push, reply
		if msg.Groups != nil && (o.groups == nil || msg.Groups.dominates(o.groups)) {
			o.groups = msg.Groups
		}
		for _, n := range msg.Nodes {
			if n == nil {
				continue
			}
			if cur := o.nodes[n.NodeID]; cur == nil || n.Version >= cur.Version {
				o.nodes[n.NodeID] = n
			}
		}
		if msg.Node != nil {
			if cur := o.nodes[msg.Node.NodeID]; cur == nil || msg.Node.Version >= cur.Version {
				o.nodes[msg.Node.NodeID] = msg.Node
			}
		}
		for _, g := range msg.Wants {
			o.replyWants[g] = struct{}{}
		}
	}
}

// takePriority removes and returns the next priority-class message.
func (o *outbox) takePriority() *syncMessage {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.directed) > 0 {
		m := o.directed[0]
		o.directed = o.directed[1:]
		return m
	}
	if len(o.creds)+len(o.revs)+len(o.leases)+len(o.policies) == 0 {
		return nil
	}
	m := &syncMessage{Type: "push"}
	for _, r := range o.creds {
		m.Creds = append(m.Creds, r)
	}
	for _, r := range o.revs {
		m.Revocations = append(m.Revocations, r)
	}
	for _, l := range o.leases {
		m.Leases = append(m.Leases, l)
	}
	for _, p := range o.policies {
		m.Policies = append(m.Policies, p)
	}
	sort.Slice(m.Creds, func(i, j int) bool {
		return credDigestKey(digestOfCred(m.Creds[i])) < credDigestKey(digestOfCred(m.Creds[j]))
	})
	o.creds = make(map[string]*credRecord)
	o.revs = make(map[string]*credRevocation)
	o.leases = make(map[string]*refresherLease)
	o.policies = make(map[string]*groupPolicy)
	return m
}

// takeBulk removes and returns the next bulk-class message.
func (o *outbox) takeBulk() *syncMessage {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.announce != nil {
		m := o.announce
		o.announce = nil
		return m
	}
	if len(o.pullWants) > 0 || o.wantRevs {
		m := &syncMessage{Type: "pull", WantRevs: o.wantRevs}
		for g := range o.pullWants {
			m.Wants = append(m.Wants, g)
		}
		sort.Strings(m.Wants)
		o.pullWants = make(map[string]struct{})
		o.wantRevs = false
		return m
	}
	if o.groups != nil || len(o.nodes) > 0 || len(o.replyWants) > 0 {
		m := &syncMessage{Type: "reply", Groups: o.groups}
		for _, n := range o.nodes {
			m.Nodes = append(m.Nodes, n)
		}
		sort.Slice(m.Nodes, func(i, j int) bool { return m.Nodes[i].NodeID < m.Nodes[j].NodeID })
		for g := range o.replyWants {
			m.Wants = append(m.Wants, g)
		}
		sort.Strings(m.Wants)
		o.groups = nil
		o.nodes = make(map[string]*nodeInfo)
		o.replyWants = make(map[string]struct{})
		return m
	}
	return nil
}

// depth is the number of pending items, shown as the link's queue depth.
func (o *outbox) depth() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(o.creds) + len(o.revs) + len(o.leases) + len(o.policies) + len(o.directed) +
		len(o.nodes) + len(o.pullWants) + len(o.replyWants)
	if o.announce != nil {
		n++
	}
	if o.groups != nil {
		n++
	}
	if o.wantRevs {
		n++
	}
	return n
}

// absorb moves everything pending in other into o. Used when a duplicate link
// is closed in favour of this one, so nothing queued on the loser is lost.
func (o *outbox) absorb(other *outbox) {
	if other == nil || other == o {
		return
	}
	other.mu.Lock()
	pending := &syncMessage{Type: "push"}
	for _, r := range other.creds {
		pending.Creds = append(pending.Creds, r)
	}
	for _, r := range other.revs {
		pending.Revocations = append(pending.Revocations, r)
	}
	for _, l := range other.leases {
		pending.Leases = append(pending.Leases, l)
	}
	for _, p := range other.policies {
		pending.Policies = append(pending.Policies, p)
	}
	directed := other.directed
	announce := other.announce
	other.creds = make(map[string]*credRecord)
	other.revs = make(map[string]*credRevocation)
	other.leases = make(map[string]*refresherLease)
	other.policies = make(map[string]*groupPolicy)
	other.directed = nil
	other.announce = nil
	other.mu.Unlock()

	o.put(pending)
	for _, d := range directed {
		o.put(d)
	}
	if announce != nil {
		o.put(announce)
	}
}
