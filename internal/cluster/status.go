package cluster

import (
	"sort"
	"time"
)

// This file builds the redaction-safe overview the admin UI renders: the full
// peer roster with liveness, every sync group with its members and current
// credential state, live connections, recent activity, and rollup stats. No
// secrets (credential payloads) are ever included.

// MemberView is one storage participating in a group, annotated for display.
type MemberView struct {
	NodeID    string `json:"node_id"`
	Label     string `json:"label"`
	MountPath string `json:"mount_path"`
	Online    bool   `json:"online"`
	Present   bool   `json:"present"`   // the node currently advertises this mount
	IsOrigin  bool   `json:"is_origin"` // authored the group's current credential
	Self      bool   `json:"self"`
	// HeldVersion/HeldHash: the credential pair this mount is using (catalogue
	// version of that pair, 0 = unknown). Laggard marks an online member whose
	// held version is behind the group's current one.
	HeldVersion uint64 `json:"held_version"`
	HeldHash    string `json:"held_hash"`
	Laggard     bool   `json:"laggard"`
	// Proto is the member node's protocol level (0 = unknown, 1 = predates
	// refresher leases: it still refreshes on its own).
	Proto     int  `json:"proto"`
	Refresher bool `json:"refresher"` // currently holds the refresher lease
	Preferred bool `json:"preferred"` // the admin's preferred refresher
}

// RefresherView is the group's refresher lease as this node sees it.
type RefresherView struct {
	NodeID    string `json:"node_id"`
	Label     string `json:"label"`
	Self      bool   `json:"self"`
	Epoch     uint64 `json:"epoch"`
	Valid     bool   `json:"valid"`      // lease currently designates a holder
	ExpiresIn int64  `json:"expires_in"` // seconds until it lapses without renewal (<=0: lapsed)
	TTLSec    int64  `json:"ttl_sec"`
}

// RefreshView is the holder's last refresh attempt for the group.
type RefreshView struct {
	Time      int64  `json:"time"`
	NodeID    string `json:"node_id"`
	Label     string `json:"label"`
	Mount     string `json:"mount"`
	Trigger   string `json:"trigger"` // API error code, proactive, refresh-request, recover
	Outcome   string `json:"outcome"` // ok | api_error | transport_error
	ErrCode   int64  `json:"err_code"`
	ExpiresIn int64  `json:"expires_in"` // lifetime of the access token obtained
}

// GroupView is a sync group plus its current (no-secret) credential state.
type GroupView struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Members   []MemberView `json:"members"`
	Fields    []string     `json:"fields"`     // credential field names being synced
	CredHash  string       `json:"cred_hash"`  // short, for "same/different" comparison
	Version   uint64       `json:"version"`    // credential record version
	Origin    string       `json:"origin"`     // node id that authored the credential
	UpdatedAt int64        `json:"updated_at"` // last credential change
	HasCred   bool         `json:"has_cred"`   // a credential has been observed/shared
	// ExpiresAt is when the newest credential's access token lapses (unix
	// seconds, 0 = unknown).
	ExpiresAt int64 `json:"expires_at"`
	// RefreshGroup: the group takes part in single-refresher leases.
	RefreshGroup bool           `json:"refresh_group"`
	Refresher    *RefresherView `json:"refresher,omitempty"`
	Preferred    string         `json:"preferred"` // preferred refresher node id
	LastRefresh  *RefreshView   `json:"last_refresh,omitempty"`
	Requests     []RequestView  `json:"requests"` // recent refresh-requests for the group
	// Mixed: a member runs a build without leases and still refreshes on its own.
	Mixed bool `json:"mixed"`
}

// NodeView is one cluster node with its advertised storages and liveness.
type NodeView struct {
	NodeID   string        `json:"node_id"`
	Label    string        `json:"label"`
	Addr     string        `json:"addr"`
	Self     bool          `json:"self"`
	Online   bool          `json:"online"`
	LastSeen int64         `json:"last_seen"`
	Storages []storageInfo `json:"storages"`
}

// ConnView is one live peer connection.
type ConnView struct {
	NodeID   string `json:"node_id"`
	Addr     string `json:"addr"`
	Outbound bool   `json:"outbound"`
	Since    int64  `json:"since"`
	// Queue depth is the number of pending (coalesced) items, RTT the last ping
	// round trip, LastAntiEntropy the last hello/announce/reply received.
	QueueDepth      int   `json:"queue_depth"`
	RTTMs           int64 `json:"rtt_ms"`
	LastAntiEntropy int64 `json:"last_anti_entropy"`
	Proto           int   `json:"proto"`
}

// LinkView is the health of the link to one peer, connected or not: what the
// admin needs to tell a flapping or silent link from a healthy one.
type LinkView struct {
	NodeID           string `json:"node_id"`
	Label            string `json:"label"`
	Connected        bool   `json:"connected"`
	Outbound         bool   `json:"outbound"`
	Since            int64  `json:"since"`
	QueueDepth       int    `json:"queue_depth"`
	RTTMs            int64  `json:"rtt_ms"`
	LastAntiEntropy  int64  `json:"last_anti_entropy"`
	Connects         int    `json:"connects"`
	Disconnects      int    `json:"disconnects"`
	Flaps10m         int    `json:"flaps_10m"` // disconnects in the last 10 minutes
	LastReason       string `json:"last_reason"`
	LastDisconnectAt int64  `json:"last_disconnect_at"`
	Proto            int    `json:"proto"`
}

// EventView is one entry in the activity log.
type EventView struct {
	Time    int64  `json:"time"`
	Kind    string `json:"kind"` // share | apply | groups | error
	GroupID string `json:"group_id"`
	Detail  string `json:"detail"`
}

// StatsView is the rollup shown at the top of the cluster panel.
type StatsView struct {
	NodesTotal  int `json:"nodes_total"`
	NodesOnline int `json:"nodes_online"`
	GroupsTotal int `json:"groups_total"`
	CredsTotal  int `json:"creds_total"`
	Connections int `json:"connections"`
}

// Status is the complete admin overview of the cluster.
type Status struct {
	NodeID      string        `json:"node_id"`
	Label       string        `json:"label"`
	Addr        string        `json:"addr"`
	Enabled     bool          `json:"enabled"`
	Active      bool          `json:"active"`
	Nodes       []NodeView    `json:"nodes"`
	Groups      []GroupView   `json:"groups"`
	Connections []ConnView    `json:"connections"`
	Links       []LinkView    `json:"links"`
	Requests    []RequestView `json:"requests"`
	Proto       int           `json:"proto"`
	RevDigest   string        `json:"rev_digest"`
	Events      []EventView   `json:"events"`
	Stats       StatsView     `json:"stats"`
}

// isOnline reports whether a node id has a live connection or was heard from
// recently.
func (m *Manager) isOnline(nodeID string, seenAt int64) bool {
	if nodeID == m.id.NodeID {
		return true
	}
	if m.conns.hasNode(nodeID) {
		return true
	}
	return seenAt > 0 && now()-seenAt < peerLivenessSec
}

// Status returns a redaction-safe snapshot for the admin UI. Node storage
// statuses are read from the store, which only ever holds publicStatus values.
func (m *Manager) Status() Status {
	cfg := m.cfgStore.get()

	// Roster: inventory + a freshly-computed self entry.
	self := m.selfNodeInfo()
	byNode := map[string]nodeInfo{}
	for _, n := range m.state.inventoryList() {
		byNode[n.NodeID] = n
	}
	selfEntry := *self
	selfEntry.seenAt = now()
	byNode[self.NodeID] = selfEntry

	mountPresent := func(nodeID, mount string) bool {
		n, ok := byNode[nodeID]
		if !ok {
			return false
		}
		for _, s := range n.Storages {
			if s.MountPath == mount {
				return true
			}
		}
		return false
	}
	labelOf := func(nodeID string) string {
		if n, ok := byNode[nodeID]; ok && n.Label != "" {
			return n.Label
		}
		return shortNode(nodeID)
	}

	nodes := make([]NodeView, 0, len(byNode))
	online := 0
	for id, n := range byNode {
		isSelf := id == m.id.NodeID
		on := m.isOnline(id, n.seenAt)
		if on {
			online++
		}
		lbl := n.Label
		if lbl == "" {
			lbl = shortNode(id)
		}
		nodes = append(nodes, NodeView{
			NodeID:   id,
			Label:    lbl,
			Addr:     n.Addr,
			Self:     isSelf,
			Online:   on,
			LastSeen: n.seenAt,
			Storages: n.Storages,
		})
	}
	sortNodeViews(nodes)
	// Groups + credential state.
	groups := make([]GroupView, 0)
	for _, g := range m.state.groupList() {
		gv := GroupView{ID: g.ID, Name: g.Name}
		if cr, ok := m.state.getCred(g.ID); ok {
			gv.HasCred = true
			gv.Fields = cr.Fields
			gv.CredHash = shortHash(cr.CredHash)
			gv.Version = cr.Version
			gv.Origin = cr.Origin
			gv.UpdatedAt = cr.UpdatedAt
			if cr.ObtainedAt > 0 && cr.ExpiresIn > 0 {
				gv.ExpiresAt = cr.ObtainedAt + cr.ExpiresIn
			}
		}
		gv.RefreshGroup = m.isRefreshGroup(g)
		holder := ""
		if gv.RefreshGroup {
			if e := m.state.leaseOf(g.ID); e != nil {
				ttl := time.Duration(e.lease.TTLSec) * time.Second
				left := ttl - time.Since(e.seenAt)
				gv.Refresher = &RefresherView{
					NodeID: e.lease.Holder, Label: labelOf(e.lease.Holder), Self: e.lease.Holder == m.id.NodeID,
					Epoch: e.lease.Epoch, Valid: e.valid(time.Now()), ExpiresIn: int64(left / time.Second), TTLSec: e.lease.TTLSec,
				}
				if gv.Refresher.Valid {
					holder = e.lease.Holder
				}
				if l := e.lease.Last; l != nil {
					gv.LastRefresh = &RefreshView{
						Time: l.Time, NodeID: e.lease.Holder, Label: labelOf(e.lease.Holder), Mount: l.Mount,
						Trigger: l.Trigger, Outcome: l.Outcome, ErrCode: l.ErrCode, ExpiresIn: l.ExpiresIn,
					}
				}
			}
			for _, r := range m.requestLog() {
				if r.GroupID == g.ID {
					gv.Requests = append(gv.Requests, r)
				}
			}
		}
		if gv.Requests == nil {
			gv.Requests = []RequestView{}
		}
		if p := m.state.policyOf(g.ID); p != nil {
			gv.Preferred = p.Preferred
		}
		for _, mem := range g.Members {
			n := byNode[mem.NodeID]
			mv := MemberView{
				NodeID:    mem.NodeID,
				Label:     labelOf(mem.NodeID),
				MountPath: mem.MountPath,
				Online:    m.isOnline(mem.NodeID, n.seenAt),
				Present:   mountPresent(mem.NodeID, mem.MountPath),
				IsOrigin:  gv.HasCred && mem.NodeID == gv.Origin,
				Self:      mem.NodeID == m.id.NodeID,
				Refresher: holder != "" && mem.NodeID == holder,
				Preferred: gv.Preferred != "" && mem.NodeID == gv.Preferred,
				Proto:     m.peerProtoOf(mem.NodeID),
			}
			for _, st := range n.Storages {
				if st.MountPath == mem.MountPath {
					mv.HeldVersion, mv.HeldHash = st.CredVer, st.CredHash
				}
			}
			mv.Laggard = gv.HasCred && mv.Online && mv.Present && mv.HeldVersion < gv.Version
			if gv.RefreshGroup && !mv.Self && mv.Proto == 1 {
				gv.Mixed = true
			}
			gv.Members = append(gv.Members, mv)
		}
		groups = append(groups, gv)
	}

	// Connections and per-peer link health.
	var conns []ConnView
	live := map[string]*peerConn{}
	for _, c := range m.conns.all() {
		conns = append(conns, ConnView{
			NodeID: c.node(), Addr: c.addr, Outbound: c.outbound, Since: c.since,
			QueueDepth: c.queueDepth(), RTTMs: c.rttNanos.Load() / int64(time.Millisecond),
			LastAntiEntropy: c.lastAE.Load() / int64(time.Second), Proto: int(c.peerProto.Load()),
		})
		if id := c.node(); id != "" {
			live[id] = c
		}
	}
	linkIDs := map[string]bool{}
	for id := range live {
		linkIDs[id] = true
	}
	for _, id := range m.links.nodeIDs() {
		linkIDs[id] = true
	}
	links := make([]LinkView, 0, len(linkIDs))
	for id := range linkIDs {
		st, flaps := m.links.snapshot(id, 600)
		lv := LinkView{
			NodeID: id, Label: labelOf(id), Connects: st.Connects, Disconnects: st.Disconnects, Flaps10m: flaps,
			LastReason: st.LastReason, LastDisconnectAt: st.LastDisconnectAt, Proto: m.peerProtoOf(id),
		}
		if c := live[id]; c != nil {
			lv.Connected, lv.Outbound, lv.Since = true, c.outbound, c.since
			lv.QueueDepth = c.queueDepth()
			lv.RTTMs = c.rttNanos.Load() / int64(time.Millisecond)
			lv.LastAntiEntropy = c.lastAE.Load() / int64(time.Second)
		}
		links = append(links, lv)
	}
	sort.Slice(links, func(i, j int) bool { return links[i].NodeID < links[j].NodeID })

	return Status{
		NodeID:      m.id.NodeID,
		Label:       cfg.Label,
		Addr:        trimURL(cfg.Addr),
		Enabled:     cfg.Enabled,
		Active:      cfg.active(),
		Nodes:       nodes,
		Groups:      groups,
		Connections: conns,
		Links:       links,
		Requests:    m.requestLog(),
		Proto:       protoLevel,
		RevDigest:   m.state.revocationDigest(),
		Events:      m.eventList(),
		Stats: StatsView{

			NodesTotal:  len(nodes),
			NodesOnline: online,
			GroupsTotal: len(groups),
			CredsTotal:  len(m.state.credSnapshot()),
			Connections: len(conns),
		},
	}
}

func sortNodeViews(ns []NodeView) {
	// self first, then by label/id.
	for i := 1; i < len(ns); i++ {
		for j := i; j > 0; j-- {
			a, b := ns[j-1], ns[j]
			less := false
			if b.Self && !a.Self {
				less = true
			} else if a.Self == b.Self {
				less = b.Label < a.Label
			}
			if less {
				ns[j-1], ns[j] = ns[j], ns[j-1]
			} else {
				break
			}
		}
	}
}
