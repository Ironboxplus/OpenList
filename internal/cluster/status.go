package cluster

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
	NodeID      string      `json:"node_id"`
	Label       string      `json:"label"`
	Addr        string      `json:"addr"`
	Enabled     bool        `json:"enabled"`
	Active      bool        `json:"active"`
	Nodes       []NodeView  `json:"nodes"`
	Groups      []GroupView `json:"groups"`
	Connections []ConnView  `json:"connections"`
	Events      []EventView `json:"events"`
	Stats       StatsView   `json:"stats"`
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

// Status returns a redaction-safe snapshot for the admin UI.
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
		}
		for _, mem := range g.Members {
			n := byNode[mem.NodeID]
			gv.Members = append(gv.Members, MemberView{
				NodeID:    mem.NodeID,
				Label:     labelOf(mem.NodeID),
				MountPath: mem.MountPath,
				Online:    m.isOnline(mem.NodeID, n.seenAt),
				Present:   mountPresent(mem.NodeID, mem.MountPath),
				IsOrigin:  gv.HasCred && mem.NodeID == gv.Origin,
				Self:      mem.NodeID == m.id.NodeID,
			})
		}
		groups = append(groups, gv)
	}

	// Connections.
	var conns []ConnView
	for _, c := range m.conns.all() {
		conns = append(conns, ConnView{NodeID: c.nodeID, Addr: c.addr, Outbound: c.outbound, Since: c.since})
	}

	return Status{
		NodeID:      m.id.NodeID,
		Label:       cfg.Label,
		Addr:        trimURL(cfg.Addr),
		Enabled:     cfg.Enabled,
		Active:      cfg.active(),
		Nodes:       nodes,
		Groups:      groups,
		Connections: conns,
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
