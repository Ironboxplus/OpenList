package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

const (
	// replayWindowSec bounds clock skew + in-flight time for envelope freshness.
	replayWindowSec = 300
	// wsPath is the persistent-connection endpoint peers dial.
	wsPath = "/api/cluster/ws"
	// dialReconcile is how often the dial supervisor re-checks connectivity.
	dialReconcile = 5 * time.Second
	// peerLivenessSec is how long a node is considered online after last contact
	// (absent a live connection).
	peerLivenessSec = 130
	// maxEvents bounds the in-memory activity log surfaced to the UI.
	maxEvents = 60
)

// Manager is the running cluster credential-sync engine for this node.
type Manager struct {
	dir      string
	id       *identity
	cfgStore *configStore
	state    *store
	replay   *replayCache
	conns    *connRegistry

	persistMu sync.Mutex

	dialMu  sync.Mutex
	dialing map[string]bool // peer URLs with an in-flight/live outbound dial

	invMu       sync.Mutex
	lastSelfVer uint64 // monotonic version for our own inventory entry

	evMu   sync.Mutex
	events []EventView

	stopCh chan struct{}
	once   sync.Once
}

// Default is the process-wide manager, set by Init. It is nil when cluster sync
// was never initialized.
var Default *Manager

func now() int64 { return time.Now().Unix() }

// Init constructs the manager: loads identity + config + persisted state and
// registers the storage hook so local credential changes propagate. It does NOT
// start the network loops — call Start once storages are loaded.
func Init(dataDir string) (*Manager, error) {
	dir := filepath.Join(dataDir, "cluster")
	id, err := loadOrCreateIdentity(dir)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		dir:      dir,
		id:       id,
		cfgStore: newConfigStore(dir),
		state:    newStore(),
		replay:   newReplayCache(replayWindowSec),
		conns:    newConnRegistry(),
		dialing:  make(map[string]bool),
		stopCh:   make(chan struct{}),
	}
	if _, err := m.cfgStore.loadOrInit(); err != nil {
		return nil, err
	}
	m.loadState()

	op.RegisterStorageHook(m.onStorageHook)
	Default = m
	return m, nil
}

// NodeID returns this node's stable identity string.
func (m *Manager) NodeID() string { return m.id.NodeID }

// ---- persistence ----

func (m *Manager) statePath() string { return filepath.Join(m.dir, "state.json") }

func (m *Manager) loadState() {
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		return
	}
	var ps persistedState
	if err := json.Unmarshal(b, &ps); err != nil {
		utils.Log.Warnf("[cluster] corrupt state file, ignoring: %v", err)
		return
	}
	m.state.load(ps)
	// resume our self-inventory version so peers never see us go backwards.
	for _, n := range ps.Inventory {
		if n.NodeID == m.id.NodeID && n.Version > m.lastSelfVer {
			m.lastSelfVer = n.Version
		}
	}
}

func (m *Manager) persist() {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	ps := m.state.export()
	b, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return
	}
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		utils.Log.Warnf("[cluster] failed to persist state: %v", err)
		return
	}
	_ = os.Rename(tmp, m.statePath())
}

// ---- config access ----

// GetConfig returns the current config (with the key redacted for display when
// redact is true).
func (m *Manager) GetConfig(redact bool) Config {
	c := m.cfgStore.get()
	if redact && c.Key != "" {
		c.Key = "********"
	}
	return c
}

// SetConfig persists a new config. A blank/"********" key keeps the existing key.
func (m *Manager) SetConfig(c Config) error {
	old := m.cfgStore.get()
	if c.Key == "" || c.Key == "********" {
		c.Key = old.Key
	}
	c.Seeds = cleanURLs(c.Seeds)
	c.Addr = trimURL(c.Addr)
	if err := m.cfgStore.save(c); err != nil {
		return err
	}
	// Drop all live connections so they reconnect with the new settings.
	m.conns.closeAll()
	go m.refreshInventory()
	go m.seedLocalCreds()
	return nil
}

// GroupSpec / MemberSpec are the exported, API-facing shapes for editing groups.
type MemberSpec struct {
	NodeID    string `json:"node_id"`
	MountPath string `json:"mount_path"`
}

type GroupSpec struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Members []MemberSpec `json:"members"`
}

// SetGroups replaces the cluster-shared sync-group document (admin edit) and
// propagates it. It then re-evaluates local credentials against the new groups.
func (m *Manager) SetGroups(specs []GroupSpec) error {
	groups := make([]group, 0, len(specs))
	for _, s := range specs {
		g := group{ID: s.ID, Name: s.Name}
		for _, ms := range s.Members {
			if ms.NodeID == "" || ms.MountPath == "" {
				continue
			}
			g.Members = append(g.Members, member{NodeID: ms.NodeID, MountPath: ms.MountPath})
		}
		if g.ID == "" || len(g.Members) == 0 {
			continue
		}
		groups = append(groups, g)
	}
	d := m.state.setGroups(m.id, groups, now())
	m.state.pruneCreds()
	m.persist()
	m.recordEvent("groups", "", fmt.Sprintf("updated to %d group(s)", len(groups)))
	gd := d
	m.broadcast(&syncMessage{Type: "push", Groups: &gd})
	go m.seedLocalCreds()
	return nil
}

// ---- lifecycle ----

// Start seeds inventory + credentials from local storages and launches the
// connection dialer and anti-entropy loop.
func (m *Manager) Start() {
	m.once.Do(func() {
		m.state.setLocalInventory(m.selfNodeInfo())
		go m.seedLocalCreds()
		go m.announceLoop()
		go m.dialSupervisor()
	})
}

// Stop halts background loops and closes all connections.
func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
	m.conns.closeAll()
}

// ---- self inventory ----

// selfNodeInfo builds this node's inventory entry from currently-loaded storages.
func (m *Manager) selfNodeInfo() *nodeInfo {
	cfg := m.cfgStore.get()
	var sts []storageInfo
	for _, d := range op.GetAllStorages() {
		st := d.GetStorage()
		sts = append(sts, storageInfo{MountPath: st.MountPath, Driver: st.Driver, Status: st.Status})
	}
	sort.Slice(sts, func(i, j int) bool { return sts[i].MountPath < sts[j].MountPath })

	m.invMu.Lock()
	v := uint64(now())
	if v <= m.lastSelfVer {
		v = m.lastSelfVer + 1
	}
	m.lastSelfVer = v
	m.invMu.Unlock()

	return &nodeInfo{
		NodeID:    m.id.NodeID,
		Label:     cfg.Label,
		Addr:      trimURL(cfg.Addr),
		Storages:  sts,
		Version:   v,
		UpdatedAt: now(),
	}
}

// refreshInventory rebuilds and broadcasts our inventory entry (call after a
// storage is added/removed or its status changes).
func (m *Manager) refreshInventory() {
	if !m.cfgStore.get().active() {
		return
	}
	self := m.selfNodeInfo()
	m.state.setLocalInventory(self)
	m.broadcast(m.announceMessage())
}

// knownNodesForPEX returns inventory entries that advertise a dialable address,
// so peers can auto-discover the rest of the mesh.
func (m *Manager) knownNodesForPEX() []*nodeInfo {
	var out []*nodeInfo
	for _, n := range m.state.inventoryList() {
		if n.Addr == "" {
			continue
		}
		cp := n
		out = append(out, &cp)
	}
	return out
}

// helloMessage greets a freshly-connected peer with everything needed to
// converge: our inventory, known dialable peers (PEX), the groups doc, and our
// credential digests.
func (m *Manager) helloMessage() *syncMessage {
	self := m.selfNodeInfo()
	m.state.setLocalInventory(self)
	gd := m.state.groupDoc()
	return &syncMessage{
		Type:        "hello",
		Node:        self,
		Nodes:       m.knownNodesForPEX(),
		Groups:      &gd,
		GroupsVer:   gd.Version,
		CredDigests: m.offerableDigests(),
	}
}

// announceMessage is the periodic anti-entropy + inventory advert.
func (m *Manager) announceMessage() *syncMessage {
	self := m.selfNodeInfo()
	gd := m.state.groupDoc()
	return &syncMessage{
		Type:        "announce",
		Node:        self,
		Nodes:       m.knownNodesForPEX(),
		Groups:      &gd,
		GroupsVer:   gd.Version,
		CredDigests: m.offerableDigests(),
	}
}

// announceLoop periodically rebroadcasts inventory + digests for convergence.
func (m *Manager) announceLoop() {
	for {
		cfg := m.cfgStore.get()
		interval := time.Duration(cfg.announceInterval()) * time.Second
		select {
		case <-m.stopCh:
			return
		case <-time.After(interval):
		}
		if !m.cfgStore.get().active() {
			continue
		}
		m.state.setLocalInventory(m.selfNodeInfo())
		m.broadcast(m.announceMessage())
	}
}

// ---- credential seeding / hooks ----

// seedLocalCreds records credentials for local healthy mounts that belong to a
// group, pulls for member-groups we have no credential for yet, and re-applies
// any held credential to local mounts (e.g. after a groups change).
func (m *Manager) seedLocalCreds() {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		return
	}
	selfID := m.id.NodeID
	var changed bool
	for _, d := range op.GetAllStorages() {
		st := d.GetStorage()
		groups := m.state.groupsForMount(selfID, st.MountPath)
		if len(groups) == 0 {
			continue
		}
		if st.Status != op.WORK {
			// Unhealthy mount (e.g. token dead at boot): drop our own stale cred so
			// it can't dominate a peer's valid one, then pull a fresh credential.
			for _, g := range groups {
				if m.state.dropOwnCred(g.ID, m.id.NodeID) {
					changed = true
				}
				m.pullGroup(g.ID)
			}
			continue
		}
		creds := extractCreds(st.Addition)
		credID := credHash(creds)
		for _, g := range groups {
			if m.state.hasCredHash(g.ID, credID) {
				m.state.markCandidateHealthy(g.ID, credID, now())
				continue // keep the original peer signature when the token is identical
			}
			if rec, ok := m.state.localCredChange(m.id, g.ID, st.Driver, st.MountPath, creds, now()); ok {
				m.state.markCandidateHealthy(g.ID, rec.CredHash, now())
				changed = true
				m.recordEvent("share", g.ID, fmt.Sprintf("%s shared %d credential field(s)", st.MountPath, len(rec.Fields)))
				m.broadcast(&syncMessage{Type: "push", Creds: []*credRecord{rec}})
			}
		}
	}
	// member-groups we hold no credential for: ask peers.
	for _, g := range m.state.groupList() {
		if len(g.mountsForNode(selfID)) == 0 {
			continue
		}
		if len(m.state.credsForGroup(g.ID)) == 0 {
			m.pullGroup(g.ID)
		}
	}
	if changed {
		m.persist()
	}
}

// onStorageHook reacts to local storage lifecycle/credential changes.
func (m *Manager) onStorageHook(typ string, d driver.Driver) {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		return
	}
	st := d.GetStorage()
	// Any storage change may alter our inventory (added/removed mount, status).
	go m.refreshInventory()

	selfID := m.id.NodeID
	groups := m.state.groupsForMount(selfID, st.MountPath)
	if len(groups) == 0 {
		return
	}
	switch typ {
	case "add", "update", "token-valid":
		// "token-valid" is fired when an authenticated request just proved the
		// token good — (re)share it so peers converge on the working credential.
		if st.Status != op.WORK {
			// Health gating: never propagate a broken/expired token. Try to recover
			// a good one from peers instead.
			for _, g := range groups {
				m.pullGroup(g.ID)
			}
			return
		}
		creds := extractCreds(st.Addition)
		credID := credHash(creds)
		var dirty bool
		for _, g := range groups {
			if m.state.hasCredHash(g.ID, credID) {
				m.state.markCandidateHealthy(g.ID, credID, now())
				continue
			}
			if rec, ok := m.state.localCredChange(m.id, g.ID, st.Driver, st.MountPath, creds, now()); ok {
				m.state.markCandidateHealthy(g.ID, rec.CredHash, now())
				dirty = true
				m.recordEvent("share", g.ID, fmt.Sprintf("%s refreshed credentials", st.MountPath))
				m.broadcast(&syncMessage{Type: "push", Creds: []*credRecord{rec}})
			}
		}
		if dirty {
			m.persist()
		}
	case "del":
		// A mount was removed locally. We keep the group's credential record (other
		// members still rely on it); only our inventory changes (handled above).
	case "token-invalid":
		// Our token died. Drop our own (now-stale) credential record so its Lamport
		// version can't out-rank a peer's valid one, then pull a fresh credential
		// from a healthy peer.
		var dropped bool
		credID := credHash(extractCreds(st.Addition))
		for _, g := range groups {
			m.state.forgetCandidateHealth(g.ID, credID)
			if m.state.dropOwnCred(g.ID, m.id.NodeID) {
				dropped = true
				m.recordEvent("invalidate", g.ID,
					fmt.Sprintf("%s token invalid — dropped local cred, pulling from peers", st.MountPath))
			}
			m.pullGroup(g.ID)
		}
		if dropped {
			m.persist()
		}
	}
}

// pullGroup asks peers for the latest credential of a group.
func (m *Manager) pullGroup(groupID string) {
	if !m.cfgStore.get().active() {
		return
	}
	m.broadcast(&syncMessage{Type: "pull", Wants: []string{groupID}})
}

// hasHealthyLocalCandidate reports whether this node is currently using this
// exact credential successfully. A signed record received from a peer is not
// evidence that it works on this machine.
func (m *Manager) hasHealthyLocalCandidate(r *credRecord) bool {
	if r == nil || !m.state.candidateHealthy(r.GroupID, r.CredHash, now()) {
		return false
	}
	g, ok := m.state.groupByID(r.GroupID)
	if !ok {
		return false
	}
	for _, mp := range g.mountsForNode(m.id.NodeID) {
		d, err := op.GetStorageByMountPath(mp)
		if err != nil {
			continue
		}
		st := d.GetStorage()
		if st.Status != op.WORK {
			continue
		}
		if r.OriginDriver != "" && st.Driver != r.OriginDriver {
			continue
		}
		if credHash(extractCreds(st.Addition)) == r.CredHash {
			return true
		}
	}
	return false
}

// canOfferCred reports whether this node may advertise a credential candidate.
// Receiving a record does not turn this node into a blind relay: it must first
// be proven by a matching local working mount within the health lease.
func (m *Manager) canOfferCred(r *credRecord) bool {
	return m.hasHealthyLocalCandidate(r)
}

// offerableDigests is credDigests() filtered to records this node may advertise.
func (m *Manager) offerableDigests() []credDigest {
	var out []credDigest
	for _, r := range m.state.credSnapshot() {
		if m.canOfferCred(r) {
			out = append(out, digestOfCred(r))
		}
	}
	return out
}

// applyCredRecord overlays a group's credential onto this node's member mounts.
func (m *Manager) applyCredRecord(r *credRecord) {
	if r == nil {
		return
	}
	cfg := m.cfgStore.get()
	if !cfg.ApplyRemote {
		return
	}
	g, ok := m.state.groupByID(r.GroupID)
	if !ok {
		return
	}
	ctx := context.Background()
	for _, mp := range g.mountsForNode(m.id.NodeID) {
		d, err := op.GetStorageByMountPath(mp)
		if err != nil {
			continue
		}
		st := *d.GetStorage() // copy; preserve ID/Status/local fields
		if r.OriginDriver != "" && st.Driver != r.OriginDriver {
			utils.Log.Warnf("[cluster] skip applying %s creds to %s: driver mismatch (%s != %s)",
				r.GroupID, mp, st.Driver, r.OriginDriver)
			continue
		}
		if st.Status == op.WORK {
			// A peer's newer Lamport value is a recovery candidate, not authority
			// to overwrite credentials this mount has already proved locally.
			continue
		}
		newAdd, changed := applyCreds(st.Addition, r.Payload)
		if !changed {
			continue // already has these credentials — no churn, no re-init
		}
		st.Addition = newAdd
		if err := op.UpdateStorage(ctx, st); err != nil {
			utils.Log.Warnf("[cluster] apply creds to %s failed: %v", mp, err)
			continue
		}
		m.recordEvent("apply", r.GroupID, fmt.Sprintf("%s adopted credentials from %s", mp, shortNode(r.Origin)))
		utils.Log.Infof("[cluster] applied group %s credentials to %s (v%d from %s)", r.GroupID, mp, r.Version, r.Origin)
	}
}

// ---- absorb (merge) helpers ----

func (m *Manager) absorbInventory(nodes []*nodeInfo) []*nodeInfo {
	var merged []*nodeInfo
	for _, n := range nodes {
		if n == nil || n.NodeID == "" || n.NodeID == m.id.NodeID {
			continue
		}
		if m.state.mergeInventory(n, now()) {
			merged = append(merged, n)
		}
	}
	return merged
}

func (m *Manager) absorbGroups(d *groupDoc) bool {
	if d == nil || !d.verify() {
		return false
	}
	if m.state.mergeGroups(d) {
		m.state.pruneCreds()
		m.persist()
		m.recordEvent("groups", "", fmt.Sprintf("received %d group(s) from %s", len(d.Groups), shortNode(d.Origin)))
		go m.seedLocalCreds()
		return true
	}
	return false
}

func (m *Manager) absorbCreds(recs []*credRecord) []*credRecord {
	var merged []*credRecord
	for _, r := range recs {
		if r == nil || !r.verify() || !m.credentialAuthorized(r) {
			continue
		}
		if m.state.mergeCred(r) {
			merged = append(merged, r)
			m.applyCredRecord(r)
		}
	}
	if len(merged) > 0 {
		m.persist()
	}
	return merged
}

// credentialAuthorized verifies that a record signer is an explicit member of
// the signed group at the exact mount it claims. The signature proves who wrote
// the record; the group document proves that writer may supply this group.
func (m *Manager) credentialAuthorized(r *credRecord) bool {
	if r == nil || r.GroupID == "" || r.Origin == "" || r.OriginMount == "" {
		return false
	}
	g, ok := m.state.groupByID(r.GroupID)
	if !ok {
		return false
	}
	for _, member := range g.Members {
		if member.NodeID == r.Origin && member.MountPath == r.OriginMount {
			return true
		}
	}
	return false
}

// ---- anti-entropy reply ----

// buildReply answers a peer's digests / wants, telling it what we hold that it
// lacks and requesting what it holds that we lack.
func (m *Manager) buildReply(in *syncMessage) *syncMessage {
	reply := &syncMessage{Type: "reply"}

	// Groups: if ours is newer, offer it.
	local := m.state.groupDoc()
	if local.Version > in.GroupsVer {
		reply.Groups = &local
	}

	// Credential anti-entropy is per (group, origin) candidate. A group-level
	// LWW exchange would silently promote one member into a primary and discard
	// the independent recovery paths held by the other members.
	peerHas := make(map[string]credDigest, len(in.CredDigests))
	wanted := make(map[string]struct{})
	offered := make(map[string]struct{})
	appendWant := func(groupID string) {
		if groupID == "" {
			return
		}
		if _, exists := wanted[groupID]; exists {
			return
		}
		wanted[groupID] = struct{}{}
		reply.Wants = append(reply.Wants, groupID)
	}
	appendCred := func(r *credRecord) {
		if r == nil || !m.canOfferCred(r) {
			return
		}
		key := credDigestKey(digestOfCred(r))
		if _, exists := offered[key]; exists {
			return
		}
		offered[key] = struct{}{}
		reply.Creds = append(reply.Creds, r)
	}
	for _, dg := range in.CredDigests {
		if dg.GroupID == "" || dg.Origin == "" {
			continue
		}
		key := credDigestKey(dg)
		peerHas[key] = dg
		cur, ok := m.state.getCredForOrigin(dg.GroupID, dg.Origin)
		if !ok {
			appendWant(dg.GroupID)
			continue
		}
		mine := digestOfCred(cur)
		switch {
		case credDigestDominates(mine, dg):
			if m.canOfferCred(cur) {
				appendCred(cur)
			} else {
				appendWant(dg.GroupID)
			}
		case credDigestDominates(dg, mine):
			appendWant(dg.GroupID)
		}
	}
	// Records we hold the peer never mentioned.
	for _, cur := range m.state.credSnapshot() {
		if _, seen := peerHas[credDigestKey(digestOfCred(cur))]; !seen {
			appendCred(cur)
		}
	}

	// An explicit pull requests every locally-proven candidate in the group.
	for _, gid := range in.Wants {
		for _, cur := range m.state.credsForGroup(gid) {
			appendCred(cur)
		}
	}

	if reply.Groups == nil && len(reply.Creds) == 0 && len(reply.Wants) == 0 {
		return nil
	}
	return reply
}

// ---- networking ----

// seal wraps a message in an encrypted envelope addressed from this node.
func (m *Manager) seal(msg *syncMessage) ([]byte, error) {
	key, err := deriveAEADKey([]byte(m.cfgStore.get().Key))
	if err != nil {
		return nil, err
	}
	return sealEnvelope(key, m.id, msg, now())
}

// sendTo enqueues a sealed message on a single connection.
func (m *Manager) sendTo(c *peerConn, msg *syncMessage) {
	frame, err := m.seal(msg)
	if err != nil {
		return
	}
	c.enqueue(frame)
}

// broadcast enqueues a sealed message on every live connection.
func (m *Manager) broadcast(msg *syncMessage) {
	conns := m.conns.all()
	if len(conns) == 0 {
		return
	}
	frame, err := m.seal(msg)
	if err != nil {
		return
	}
	for _, c := range conns {
		c.enqueue(frame)
	}
}

// relayMsg forwards newly-merged info to every connection except the one it
// arrived on — this is what lets two NAT'd nodes converge through a common
// reachable peer. Signed records/groups stay authentic across the relay.
func (m *Manager) relayMsg(msg *syncMessage, except *peerConn) {
	conns := m.conns.all()
	if len(conns) <= 1 {
		return
	}
	frame, err := m.seal(msg)
	if err != nil {
		return
	}
	for _, c := range conns {
		if c == except {
			continue
		}
		c.enqueue(frame)
	}
}

// handleFrame authenticates and processes one inbound frame. A frame that fails
// to open (wrong key / replay / stale) drops the connection.
func (m *Manager) handleFrame(c *peerConn, data []byte) {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		c.close()
		return
	}
	key, err := deriveAEADKey([]byte(cfg.Key))
	if err != nil {
		c.close()
		return
	}
	env, msg, err := openEnvelope(key, data, now(), m.replay, replayWindowSec)
	if err != nil {
		utils.Log.Debugf("[cluster] frame rejected from %s: %v", c.nodeID, err)
		c.close()
		return
	}
	if env.Sender == m.id.NodeID {
		c.close() // connected to ourselves
		return
	}
	if c.nodeID == "" {
		m.conns.bind(c, env.Sender)
	}

	relay := &syncMessage{Type: "push"}
	relayHas := false

	// Inventory + PEX.
	var invs []*nodeInfo
	if msg.Node != nil {
		invs = append(invs, msg.Node)
	}
	invs = append(invs, msg.Nodes...)
	if merged := m.absorbInventory(invs); len(merged) > 0 {
		relay.Nodes = merged
		relayHas = true
	}
	// Groups.
	if m.absorbGroups(msg.Groups) {
		gd := m.state.groupDoc()
		relay.Groups = &gd
		relayHas = true
	}
	// Credentials.
	if merged := m.absorbCreds(msg.Creds); len(merged) > 0 {
		// A relay must not amplify a peer credential merely because its signature
		// was valid. Only candidates that this node has actually proven locally
		// may leave this node again.
		for _, r := range merged {
			if m.canOfferCred(r) {
				relay.Creds = append(relay.Creds, r)
			}
		}
		if len(relay.Creds) > 0 {
			relayHas = true
		}
	}
	if relayHas {
		m.relayMsg(relay, c)
	}

	// Anti-entropy reply only for digest/want-bearing messages (avoids echo).
	switch msg.Type {
	case "hello", "announce", "pull":
		if reply := m.buildReply(msg); reply != nil {
			m.sendTo(c, reply)
		}
	}
}

// ---- events ----

func (m *Manager) recordEvent(kind, groupID, detail string) {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	m.events = append(m.events, EventView{Time: now(), Kind: kind, GroupID: groupID, Detail: detail})
	if len(m.events) > maxEvents {
		m.events = m.events[len(m.events)-maxEvents:]
	}
}

func (m *Manager) eventList() []EventView {
	m.evMu.Lock()
	defer m.evMu.Unlock()
	out := make([]EventView, len(m.events))
	copy(out, m.events)
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func shortNode(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
