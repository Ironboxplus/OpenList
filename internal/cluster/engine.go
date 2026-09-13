package cluster

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
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
	// defaultCandidateRetryDelay matches the SDK refresh cooldown. A provider throttle
	// must not be retried immediately by a fresh probe client.
	defaultCandidateRetryDelay = 5 * time.Minute
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

	recoveryMu          sync.Mutex
	recoveryLocks       map[string]*sync.Mutex // per local mount: serialize credential adoption
	recoveryQueues      map[string]*recoveryQueue
	recoveryRetries     map[string]struct{} // (group, mount, credential) delayed retries
	candidateRetryDelay time.Duration
	probeCredentialFn   func(context.Context, model.Storage, string) error

	dialMu  sync.Mutex
	dialing map[string]bool // peer URLs with an in-flight/live outbound dial

	invMu       sync.Mutex
	lastSelfVer uint64 // monotonic version for our own inventory entry

	evMu   sync.Mutex
	events []EventView

	stopCh chan struct{}
	once   sync.Once
}

// recoveryQueue is one mount's event-driven recovery mailbox. It deliberately
// contains no timer: a pair is enqueued by an observed 401, a newly received
// signed candidate, or startup reconstruction. The source of truth remains the
// durable candidate/tombstone state, so a crash can safely rebuild this queue.
type recoveryQueue struct {
	running bool
	pending map[string]*credRecord // credential hash -> immutable signed candidate
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
		dir:             dir,
		id:              id,
		cfgStore:        newConfigStore(dir),
		state:           newStore(),
		replay:          newReplayCache(replayWindowSec),
		conns:           newConnRegistry(),
		dialing:         make(map[string]bool),
		recoveryLocks:   make(map[string]*sync.Mutex),
		recoveryQueues:  make(map[string]*recoveryQueue),
		recoveryRetries: make(map[string]struct{}),
		stopCh:          make(chan struct{}),
	}
	if _, err := m.cfgStore.loadOrInit(); err != nil {
		return nil, err
	}
	m.loadState()

	op.RegisterStorageHook(m.onStorageHook)
	op.RegisterStorageCredentialHook(m.onStorageCredential)
	op.RegisterStorageCredentialHealthHook(m.onStorageCredentialHealthy)
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

func (m *Manager) persist() error {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	ps := m.state.export()
	b, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		utils.Log.Warnf("[cluster] failed to persist state: %v", err)
		return err
	}
	if err := os.Rename(tmp, m.statePath()); err != nil {
		return err
	}
	return nil
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
	if err := m.persist(); err != nil {
		return fmt.Errorf("persist groups: %w", err)
	}
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
		Revocations: m.state.revocationSnapshot(),
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
		Revocations: m.state.revocationSnapshot(),
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

// seedLocalCreds reconciles every locally failed mount after startup or a group
// change. A persisted Storage.Status=WORK is not proof that its credential pair
// is still accepted by the provider, so startup never mints a fresh candidate
// from it; however, a persisted non-WORK mount must retry durable peer
// candidates even when they arrived before this process started.
func (m *Manager) seedLocalCreds() {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		return
	}
	selfID := m.id.NodeID
	for _, d := range op.GetAllStorages() {
		st := d.GetStorage()
		groups := m.state.groupsForMount(selfID, st.MountPath)
		if len(groups) == 0 {
			continue
		}
		for _, g := range groups {
			if st.Status != op.WORK {
				m.enqueueKnownCandidates(g.ID, st.MountPath, credHash(extractCreds(st.Addition)))
			}
			m.pullGroup(g.ID)
		}
	}
}

// revokeOwnCandidate removes the local source candidate and returns a signed
// tombstone that every relay can retain. The tombstone's version is newer than
// the removed credential, so an old relay replay cannot re-enter state while a
// future credential rotation from this source remains valid.
func (m *Manager) revokeOwnCandidate(groupID, mount string) (*credRevocation, bool) {
	return m.state.revokeOwnCred(m.id, groupID, mount, now())
}

// onStorageHook only tracks lifecycle/inventory changes. Authentication events
// are intentionally handled by onStorageCredential because they need the
// immutable Addition snapshot of the request that produced them.
func (m *Manager) onStorageHook(typ string, d driver.Driver) {
	if !m.cfgStore.get().active() || d == nil || d.GetStorage() == nil {
		return
	}
	if typ == "token-valid" || typ == "token-invalid" {
		return
	}
	go m.refreshInventory()
}

// onStorageCredential consumes a real provider result bound to the exact
// Addition used on the wire. It is the sole authority for publishing a pair or
// writing its group-wide 401 tombstone; ordinary storage update hooks are never
// authentication evidence.
func (m *Manager) onStorageCredential(typ string, event op.StorageCredentialEvent) {
	if !m.cfgStore.get().active() || event.Storage == nil {
		return
	}
	st := event.Storage.GetStorage()
	if st == nil || st.Addition != event.Addition || !st.Modified.Equal(event.Modified) {
		return // late result from an old client generation
	}
	go m.refreshInventory()
	groups := m.state.groupsForMount(m.id.NodeID, st.MountPath)
	if len(groups) == 0 {
		return
	}

	switch typ {
	case "token-valid":
		if st.Status != op.WORK {
			for _, g := range groups {
				m.pullGroup(g.ID)
			}
			return
		}
		creds := extractCreds(event.Addition)
		credID := credHash(creds)
		var dirty bool
		var shares []*credRecord
		for _, g := range groups {
			if m.state.hasCredForSource(g.ID, m.id.NodeID, st.MountPath, credID) {
				m.state.markCandidateHealthy(g.ID, credID, now())
				continue
			}
			if rec, ok := m.state.localCredChange(m.id, g.ID, st.Driver, st.MountPath, creds, now()); ok {
				m.state.markCandidateHealthy(g.ID, rec.CredHash, now())
				dirty = true
				m.recordEvent("share", g.ID, fmt.Sprintf("%s refreshed credentials", st.MountPath))
				shares = append(shares, rec)
			}
		}
		if dirty {
			if err := m.persist(); err != nil {
				utils.Log.Errorf("[cluster] not sharing unpersisted credential for %s: %v", st.MountPath, err)
				return
			}
		}
		if len(shares) > 0 {
			m.broadcast(&syncMessage{Type: "push", Creds: shares})
		}

	case "token-invalid":
		// This event is generation-bound. Any 401xxxxx invalidates the precise
		// access/refresh pair used by the request even if another asynchronous
		// success would otherwise leave Storage.Status as WORK.
		credID := credHash(extractCreds(event.Addition))
		if credID == "" {
			return
		}
		var revocations []*credRevocation
		for _, g := range groups {
			m.state.forgetCandidateHealth(g.ID, credID)
			m.state.markCandidateFailed(g.ID, st.MountPath, credID, now())
			if rev, ok := m.state.revokePair(m.id, g.ID, st.MountPath, credID, now()); ok {
				m.recordEvent("invalidate", g.ID,
					fmt.Sprintf("%s token invalid — dropped local cred, pulling from peers", st.MountPath))
				revocations = append(revocations, rev)
			} else {
				m.recordEvent("invalidate", g.ID,
					fmt.Sprintf("%s token invalid — quarantined failed peer candidate", st.MountPath))
			}
		}
		// The pair-wide 401 fact must survive a crash before a recovery worker
		// can touch any alternative. This makes Revoked(P) durable-before-apply.
		if err := m.persist(); err != nil {
			utils.Log.Errorf("[cluster] not recovering after unpersisted 401 for %s: %v", st.MountPath, err)
			return
		}
		if len(revocations) > 0 {
			m.broadcast(&syncMessage{Type: "push", Revocations: revocations})
		}
		for _, g := range groups {
			m.enqueueKnownCandidates(g.ID, st.MountPath, credID)
			m.pullGroup(g.ID)
		}
	}
}

// onStorageCredentialHealthy renews in-memory proof only for the exact pair
// that received a successful provider response. It cannot accidentally mark a
// newer pair healthy after an old request returns late.
func (m *Manager) onStorageCredentialHealthy(event op.StorageCredentialEvent) {
	if !m.cfgStore.get().active() || event.Storage == nil {
		return
	}
	st := event.Storage.GetStorage()
	if st == nil || st.Status != op.WORK || st.Addition != event.Addition || !st.Modified.Equal(event.Modified) {
		return
	}
	credID := credHash(extractCreds(event.Addition))
	if credID == "" {
		return
	}
	for _, g := range m.state.groupsForMount(m.id.NodeID, st.MountPath) {
		if m.state.hasCredHash(g.ID, credID) {
			m.state.markCandidateHealthy(g.ID, credID, now())
			m.state.clearCandidateFailed(g.ID, st.MountPath, credID)
		}
	}
}

// enqueueKnownCandidates rebuilds a failed mount's durable recovery work. It is
// used after a local 401 and after restart; it never creates candidates or
// refreshes a token.
func (m *Manager) enqueueKnownCandidates(groupID, mount, failedHash string) {
	for _, candidate := range m.state.recoveryCandidates(groupID, mount, failedHash) {
		m.enqueueCandidate(candidate, mount)
	}
}

// enqueueCandidate queues one newly learned, signed candidate for one local
// mount. A queue is keyed by credential hash, so duplicate frames and relay
// paths cannot create probe storms. The worker is per (group, mount), making
// activation deterministic and serial with storage replacement.
func (m *Manager) enqueueCandidate(candidate *credRecord, mount string) {
	if candidate == nil || mount == "" || !m.credentialAuthorized(candidate) {
		return
	}
	key := candidate.GroupID + "\x00" + mount
	m.recoveryMu.Lock()
	if m.recoveryQueues == nil {
		m.recoveryQueues = make(map[string]*recoveryQueue)
	}
	queue := m.recoveryQueues[key]
	if queue == nil {
		queue = &recoveryQueue{pending: make(map[string]*credRecord)}
		m.recoveryQueues[key] = queue
	}
	if _, exists := queue.pending[candidate.CredHash]; !exists {
		cp := *candidate
		queue.pending[candidate.CredHash] = &cp
	}
	if queue.running {
		m.recoveryMu.Unlock()
		return
	}
	queue.running = true
	m.recoveryMu.Unlock()
	go m.runCandidateRecovery(key, candidate.GroupID, mount)
}

func (m *Manager) takeCandidate(key string) *credRecord {
	m.recoveryMu.Lock()
	defer m.recoveryMu.Unlock()
	queue := m.recoveryQueues[key]
	if queue == nil {
		return nil
	}
	if len(queue.pending) == 0 {
		queue.running = false
		delete(m.recoveryQueues, key)
		return nil
	}
	hashes := make([]string, 0, len(queue.pending))
	for hash := range queue.pending {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	candidate := queue.pending[hashes[0]]
	delete(queue.pending, hashes[0])
	return candidate
}

func (m *Manager) finishCandidateRecovery(key string) {
	m.recoveryMu.Lock()
	defer m.recoveryMu.Unlock()
	delete(m.recoveryQueues, key)
}

// scheduleCandidateRetry keeps one delayed retry per credential and mount.
// 40140117 is a provider cooldown, so retrying immediately with a new SDK
// client would only extend the throttle; dropping the work item would leave a
// still-valid catalogued pair dormant until restart.
func (m *Manager) scheduleCandidateRetry(candidate *credRecord, mount string) {
	if candidate == nil || mount == "" {
		return
	}
	retryKey := candidate.GroupID + "\x00" + mount + "\x00" + candidate.CredHash
	m.recoveryMu.Lock()
	if m.recoveryRetries == nil {
		m.recoveryRetries = make(map[string]struct{})
	}
	if _, exists := m.recoveryRetries[retryKey]; exists {
		m.recoveryMu.Unlock()
		return
	}
	m.recoveryRetries[retryKey] = struct{}{}
	delay := m.candidateRetryDelay
	if delay <= 0 {
		delay = defaultCandidateRetryDelay
	}
	stopCh := m.stopCh
	cp := *candidate
	m.recoveryMu.Unlock()

	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-stopCh:
			m.recoveryMu.Lock()
			delete(m.recoveryRetries, retryKey)
			m.recoveryMu.Unlock()
			return
		}
		m.recoveryMu.Lock()
		delete(m.recoveryRetries, retryKey)
		m.recoveryMu.Unlock()
		select {
		case <-stopCh:
			return
		default:
		}
		if !m.state.hasCredHash(cp.GroupID, cp.CredHash) || m.state.candidateFailed(cp.GroupID, mount, cp.CredHash) {
			return
		}
		storage, err := op.GetStorageByMountPath(mount)
		if err != nil || storage.GetStorage().Status == op.WORK {
			return
		}
		m.enqueueCandidate(&cp, mount)
	}()
}

// runCandidateRecovery is the only place a received candidate may become the
// mount's active storage. A successful probe stops the queue; a terminal
// provider 401 emits a durable group-wide tombstone before the next candidate
// is attempted. A 40140117 refresh throttle is retryable and must keep the
// candidate catalogued.
func (m *Manager) runCandidateRecovery(key, groupID, mount string) {
	for {
		candidate := m.takeCandidate(key)
		if candidate == nil {
			return
		}
		if !m.credentialAuthorized(candidate) {
			continue
		}
		attempted, success, terminal, retryable := m.applyCredRecordToMount(candidate, mount)
		if success {
			m.finishCandidateRecovery(key)
			return
		}
		if retryable {
			m.scheduleCandidateRetry(candidate, mount)
			continue
		}
		if !attempted || !terminal {
			continue
		}
		if rev, revoked := m.state.revokePair(m.id, groupID, mount, candidate.CredHash, now()); revoked {
			m.recordEvent("invalidate", groupID,
				fmt.Sprintf("%s rejected candidate %s with provider 401", mount, shortHash(candidate.CredHash)))
			if err := m.persist(); err != nil {
				utils.Log.Errorf("[cluster] not relaying unpersisted candidate rejection for %s: %v", mount, err)
				return
			}
			m.broadcast(&syncMessage{Type: "push", Revocations: []*credRevocation{rev}})
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

// canOfferCred reports whether this node may relay a credential candidate. A
// valid member signature is the authority for transport: a relay does not need
// to prove the pair locally before forwarding it. Local proof only controls
// whether this node activates a pair on one of its mounts. Keeping those two
// facts separate is essential for NAT/accept-only topologies, where a healthy
// source and an invalid target may never have a direct connection.
func (m *Manager) canOfferCred(r *credRecord) bool {
	return m.credentialAuthorized(r)
}

// offerableDigests is the signed, non-revoked candidate inventory that may be
// relayed during anti-entropy. It is deliberately independent of local mount
// health; otherwise a hub that did not itself mount 115 would black-hole a
// healthy source's credential record.
func (m *Manager) offerableDigests() []credDigest {
	var out []credDigest
	for _, r := range m.state.credSnapshot() {
		if m.canOfferCred(r) {
			out = append(out, digestOfCred(r))
		}
	}
	return out
}

func (m *Manager) recoveryLock(mount string) *sync.Mutex {
	m.recoveryMu.Lock()
	defer m.recoveryMu.Unlock()
	if m.recoveryLocks == nil {
		m.recoveryLocks = make(map[string]*sync.Mutex)
	}
	lock := m.recoveryLocks[mount]
	if lock == nil {
		lock = &sync.Mutex{}
		m.recoveryLocks[mount] = lock
	}
	return lock
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
	for _, mp := range g.mountsForNode(m.id.NodeID) {
		m.applyCredRecordToMount(r, mp)
	}
}

// applyCredRecordToMount adopts one candidate on one local mount. It returns
// whether a probe was attempted, whether activation completed, whether the
// probe received a terminal provider-auth 401, and whether it received the
// retryable 40140117 throttle. Candidate payloads are verified by a temporary
// real driver before production storage is changed.
func (m *Manager) applyCredRecordToMount(r *credRecord, mp string) (attempted, success, terminal, retryable bool) {
	if r == nil || !m.cfgStore.get().ApplyRemote || m.state.candidateFailed(r.GroupID, mp, r.CredHash) {
		return false, false, false, false
	}
	ctx := context.Background()
	{
		lock := m.recoveryLock(mp)
		lock.Lock()
		defer lock.Unlock()
		d, err := op.GetStorageByMountPath(mp)
		if err != nil {
			return false, false, false, false
		}
		before := *d.GetStorage() // preserve a local LKG for rollback
		st := before
		if r.OriginDriver != "" && st.Driver != r.OriginDriver {
			utils.Log.Warnf("[cluster] skip applying %s creds to %s: driver mismatch (%s != %s)",
				r.GroupID, mp, st.Driver, r.OriginDriver)
			return false, false, false, false
		}
		if st.Status == op.WORK && r.Origin == m.id.NodeID {
			// A self-authored record can only reach here via a relay echo
			// (anti-entropy reply): it is never authority over itself, and
			// applyCreds below reports changed=false once the payloads match
			// anyway. Skip before paying for a probe.
			return false, false, false, false
		}
		// WORK is not proof this mount's credential pair is still accepted by
		// the provider: 115's refresh_token rotates on use, so a peer's
		// successful refresh may already have killed this mount's own pair on
		// the provider side before this node ever sees a local 401. A WORK
		// mount must therefore be able to actively adopt a peer-authored
		// candidate instead of waiting to fail on its own first — the
		// temporary-driver probe below, not local status, is what proves a
		// candidate live before it ever touches production storage.
		//
		// A live probe only proves the candidate works right now, not that it
		// is newer: access_token carries its own TTL, so a pair that already
		// lost a rotation race can still probe clean for a while even though
		// its refresh_token was already consumed by whoever rotated past it.
		// Reject a candidate whose version does not exceed what this node's
		// own catalogue already has on record for the mount's current pair —
		// otherwise a WORK mount could regress onto a dead refresh_token, and
		// two WORK nodes could oscillate adopting each other's stale pairs.
		if st.Status == op.WORK {
			if current, ok := m.state.credByHash(r.GroupID, credHash(extractCreds(st.Addition))); ok && current.Version >= r.Version {
				return false, false, false, false
			}
		}
		newAdd, changed := applyCreds(st.Addition, r.Payload)
		if !changed {
			return false, false, false, false // already has these credentials — no churn, no re-init
		}
		probe := m.probeCredentialFn
		if probe == nil {
			probe = probeCredential
		}
		if err := probe(ctx, st, newAdd); err != nil {
			utils.Log.Warnf("[cluster] probe candidate %s for %s failed: %v", shortHash(r.CredHash), mp, err)
			return true, false, isProvider401(err), isProviderRefreshThrottle(err)
		}
		st.Addition = newAdd
		if err := op.UpdateStorage(ctx, st); err != nil {
			utils.Log.Warnf("[cluster] apply creds to %s failed: %v", mp, err)
			// UpdateStorage persists before it initializes. Restore the complete
			// previous storage record rather than leaving an unproven pair behind.
			if rollbackErr := op.UpdateStorage(ctx, before); rollbackErr != nil {
				utils.Log.Errorf("[cluster] rollback %s after failed candidate commit: %v", mp, rollbackErr)
			}
			return true, false, false, false
		}
		m.state.markCandidateHealthy(r.GroupID, r.CredHash, now())
		m.recordEvent("apply", r.GroupID, fmt.Sprintf("%s adopted credentials from %s", mp, shortNode(r.Origin)))
		utils.Log.Infof("[cluster] applied group %s credentials to %s (v%d from %s)", r.GroupID, mp, r.Version, r.Origin)
		return true, true, false, false
	}
}

// probeCredential constructs the registered production driver in isolation. It
// runs that driver's actual Init path (115 Open performs UserInfo) without
// registering it in op's storage map or writing SQLite. A failed candidate thus
// never replaces the mount's active/LKG pair.
func probeCredential(ctx context.Context, storage model.Storage, addition string) error {
	constructor, err := op.GetDriver(storage.Driver)
	if err != nil {
		return err
	}
	temporary := constructor()
	probe := newProbeStorage(storage, addition)
	temporary.SetStorage(probe)
	if err := utils.Json.UnmarshalFromString(probe.Addition, temporary.GetAddition()); err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return temporary.Init(probeCtx)
}

// newProbeStorage gives a temporary driver the candidate fields but no ownership
// of the persisted storage row. Some production drivers report an authenticated
// Init through lifecycle hooks; ID=0 prevents such a callback from changing the
// live row, and WORK prevents a successful probe from emitting token-valid.
func newProbeStorage(live model.Storage, addition string) model.Storage {
	live.ID = 0
	live.Status = op.WORK
	live.Addition = addition
	return live
}

func isProvider401(err error) bool {
	var authErr *sdk.Error
	return stderrors.As(err, &authErr) && authErr.Code != sdk.CodeRefreshFrequently && sdk.Is401Started(authErr.Code)
}

func isProviderRefreshThrottle(err error) bool {
	var authErr *sdk.Error
	return stderrors.As(err, &authErr) && authErr.Code == sdk.CodeRefreshFrequently
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
		if err := m.persist(); err != nil {
			utils.Log.Errorf("[cluster] not relaying unpersisted groups: %v", err)
			return false
		}
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
		}
	}
	if len(merged) > 0 {
		if err := m.persist(); err != nil {
			utils.Log.Errorf("[cluster] not activating unpersisted received credentials: %v", err)
			return nil
		}
		for _, r := range merged {
			// A record newly arriving after a local 401 must wake recovery even if
			// the earlier empty-candidate scan already completed. The queue itself
			// deduplicates the pair and serializes the real driver probe.
			g, ok := m.state.groupByID(r.GroupID)
			if !ok {
				continue
			}
			for _, mount := range g.mountsForNode(m.id.NodeID) {
				m.enqueueCandidate(r, mount)
			}
		}
	}
	return merged
}

func (m *Manager) absorbRevocations(revs []*credRevocation) []*credRevocation {
	var merged []*credRevocation
	for _, rev := range revs {
		if !credentialRevocationAuthorizedByGroups(m.state.groupList(), rev) {
			continue
		}
		if m.state.mergeRevocation(rev) {
			merged = append(merged, rev)
			m.recordEvent("revoke", rev.GroupID,
				fmt.Sprintf("%s revoked candidate from %s", rev.OriginMount, shortNode(rev.Origin)))
		}
	}
	if len(merged) > 0 {
		if err := m.persist(); err != nil {
			utils.Log.Errorf("[cluster] not relaying unpersisted revocations: %v", err)
			return nil
		}
	}
	return merged
}

// credentialAuthorized verifies that a record signer is an explicit member of
// the signed group at the exact mount it claims. The signature proves who wrote
// the record; the group document proves that writer may supply this group.
func (m *Manager) credentialAuthorized(r *credRecord) bool {
	return credentialAuthorizedByGroups(m.state.groupList(), r)
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

	// Credential anti-entropy is per (group, origin, mount) candidate. A group-level
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
		cur, ok := m.state.getCredForSource(dg.GroupID, dg.Origin, dg.OriginMount)
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
	reply.Revocations = m.state.revocationSnapshot()

	if reply.Groups == nil && len(reply.Creds) == 0 && len(reply.Revocations) == 0 && len(reply.Wants) == 0 {
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
	// Candidate tombstones must converge before credentials: otherwise a relay
	// can briefly resurrect an old signed token while a revocation is in flight.
	if revoked := m.absorbRevocations(msg.Revocations); len(revoked) > 0 {
		relay.Revocations = revoked
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
