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
	"k8s.io/client-go/util/workqueue"
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
	candidateRetryDelay time.Duration
	probeCredentialFn   func(context.Context, model.Storage, string) error

	// The reconcile queue is created on first use (see reconcileQueue), so a
	// Manager that never saw an event costs nothing.
	savedMu sync.Mutex
	saved   map[string]savedPair // mount -> pair the storage was last updated to
	// diskClosed is set by Stop: from then on nothing is written under m.dir.
	// Storage hooks run on their own goroutines and can still fire after Stop;
	// both writers check it under the lock they hold while writing, so Stop
	// returns only after an in-flight write has finished.
	diskClosed bool // guarded by persistMu and savedMu (set holding both)
	queueMu    sync.Mutex
	queue      workqueue.TypedRateLimitingInterface[string]
	pullMu     sync.Mutex
	lastPull   map[string]time.Time // group -> last pull request, for the cooldown

	dialMu sync.Mutex
	dials  map[string]*dialState // peer URL -> redial bookkeeping

	links      linkBook // per-peer link health history
	refr       refresherState
	deliveries deliveryBook // credential versions awaiting member confirmation

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
		dir:           dir,
		id:            id,
		cfgStore:      newConfigStore(dir),
		state:         newStore(),
		replay:        newReplayCache(replayWindowSec),
		conns:         newConnRegistry(),
		dials:         make(map[string]*dialState),
		recoveryLocks: make(map[string]*sync.Mutex),
		stopCh:        make(chan struct{}),
	}
	if _, err := m.cfgStore.loadOrInit(); err != nil {
		return nil, err
	}
	m.loadState()
	m.loadSaved()
	m.state.revRetentionSec = m.cfgStore.get().revocationRetentionSec()

	op.RegisterStorageHook(m.onStorageHook)
	op.RegisterStoragePreSaveHook(m.onStoragePreSave)
	op.RegisterStorageCredentialHook(m.onStorageCredential)
	op.RegisterStorageCredentialHealthHook(m.onStorageCredentialHealthy)
	// Only the group's lease holder may spend a single-use refresh_token.
	op.SetRefreshCoordinator(m)
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
	if m.diskClosed {
		return nil
	}
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
	m.state.revRetentionSec = c.revocationRetentionSec()
	// Drop all live connections so they reconnect with the new settings.
	m.conns.closeAll(reasonConfig)
	go m.refreshInventory()
	m.enqueueAll()
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
	m.state.pruneLeases()
	if err := m.persist(); err != nil {
		return fmt.Errorf("persist groups: %w", err)
	}
	m.recordEvent("groups", "", fmt.Sprintf("updated to %d group(s)", len(groups)))
	gd := d
	m.broadcast(&syncMessage{Type: "push", Groups: &gd})
	m.enqueueAll()
	return nil
}

// ---- lifecycle ----

// Start seeds inventory + credentials from local storages and launches the
// connection dialer and anti-entropy loop.
func (m *Manager) Start() {
	m.once.Do(func() {
		m.state.setLocalInventory(m.selfNodeInfo())
		m.enqueueAll()
		go m.resyncLoop()
		go m.announceLoop()
		go m.dialSupervisor()
		go m.refresherLoop()
	})
}

// Stop halts background loops and closes all connections.
func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
	m.conns.closeAll(reasonShutdown)
	m.reconcileQueue().ShutDown()
	// No write to the cluster directory after this point (see diskClosed).
	m.persistMu.Lock()
	m.savedMu.Lock()
	m.diskClosed = true
	m.savedMu.Unlock()
	m.persistMu.Unlock()
}

// ---- self inventory ----

// selfNodeInfo builds this node's inventory entry from currently-loaded storages.
func (m *Manager) selfNodeInfo() *nodeInfo {
	cfg := m.cfgStore.get()
	var sts []storageInfo
	for _, d := range op.GetAllStorages() {
		st := d.GetStorage()
		info := storageInfoOf(*st)
		info.CredHash, info.CredVer = m.heldCred(*st)
		sts = append(sts, info)
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

// / helloMessage greets a freshly-connected peer with everything needed to
// converge: our inventory, known dialable peers (PEX), the groups doc, our
// credential digests, the refresher leases/policies and a revocation digest.
func (m *Manager) helloMessage() *syncMessage {
	msg := m.announceMessage()
	msg.Type = "hello"
	return msg
}

// announceMessage is the periodic anti-entropy + inventory advert. It carries
// only a digest of the revocation set; a peer whose digest differs pulls the
// set (see handleFrame), so the 10 KB tombstone list no longer rides on every
// announce and every reply.
func (m *Manager) announceMessage() *syncMessage {
	self := m.selfNodeInfo()
	m.state.setLocalInventory(self)
	gd := m.state.groupDoc()
	return &syncMessage{
		Type:        "announce",
		Node:        self,
		Nodes:       m.knownNodesForPEX(),
		Groups:      &gd,
		GroupsVer:   gd.Version,
		CredDigests: m.offerableDigests(),
		Proto:       protoLevel,
		RevDigest:   m.state.revocationDigest(),
		Leases:      m.state.leaseList(),
		Policies:    m.state.policyList(),
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
	// add/update/del: LoadStorage and UpdateStorage fire this after the storage
	// is registered, so a reconcile queued earlier for a not-yet-registered mount
	// is satisfied here at the latest.
	if typ == "del" {
		m.forgetSaved(d.GetStorage().MountPath)
	}
	m.enqueueMount(d.GetStorage().MountPath)
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

	// Whatever the result, the mount's desired state may now differ from its
	// actual state. The reconcile loop decides; this handler only wakes it.
	defer m.enqueueMount(st.MountPath)

	switch typ {
	case "token-valid":
		if st.Status != op.WORK {
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
			obtained, expiresIn := m.timingForPayload(st.MountPath, creds)
			if rec, ok := m.state.localCredChangeTimed(m.id, g.ID, st.Driver, st.MountPath, creds, now(), obtained, expiresIn); ok {
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
			for _, rec := range shares {
				m.trackDelivery(rec)
				utils.Log.Infof("[cluster] push group=%s v%d origin=%s mount=%s hash=%s expires_in=%ds to %d link(s)",
					rec.GroupID, rec.Version, shortNode(rec.Origin), rec.OriginMount, shortHash(rec.CredHash), rec.ExpiresIn, m.linkCount())
			}
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
			for _, rev := range revocations {
				utils.Log.Warnf("[cluster] revoke group=%s hash=%s v%d origin=%s mount=%s (provider 401 on this node) -> %d link(s)",
					rev.GroupID, shortHash(rev.CredHash), rev.Version, shortNode(rev.Origin), rev.OriginMount, m.linkCount())
			}
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

// pullGroup asks peers for the latest credential of a group.
func (m *Manager) pullGroup(groupID string) {
	if !m.cfgStore.get().active() {
		return
	}
	utils.Log.Infof("[cluster] pull group=%s requested from %d link(s)", groupID, m.linkCount())
	m.broadcast(&syncMessage{Type: "pull", Wants: []string{groupID}, Proto: protoLevel})
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
		m.enqueueAll()
		return true
	}
	return false
}

func (m *Manager) absorbCreds(recs []*credRecord) []*credRecord {
	return m.absorbCredsFrom(recs, "")
}

// absorbCredsFrom merges received credential records and logs the outcome of
// each (version, origin, source mount, hash) so a missing or ignored record is
// diagnosable from the log alone.
func (m *Manager) absorbCredsFrom(recs []*credRecord, from string) []*credRecord {
	var merged []*credRecord
	for _, r := range recs {
		if r == nil || !r.verify() || !m.credentialAuthorized(r) {
			if r != nil {
				utils.Log.Debugf("[cluster] recv cred group=%s v%d origin=%s mount=%s from=%s: REJECTED (bad signature or unauthorized source)",
					r.GroupID, r.Version, shortNode(r.Origin), r.OriginMount, shortNode(from))
			}
			continue
		}
		if m.state.mergeCred(r) {
			merged = append(merged, r)
			utils.Log.Infof("[cluster] recv cred group=%s v%d origin=%s mount=%s hash=%s from=%s: merged",
				r.GroupID, r.Version, shortNode(r.Origin), r.OriginMount, shortHash(r.CredHash), shortNode(from))
		} else {
			utils.Log.Debugf("[cluster] recv cred group=%s v%d origin=%s mount=%s hash=%s from=%s: already known/superseded/revoked",
				r.GroupID, r.Version, shortNode(r.Origin), r.OriginMount, shortHash(r.CredHash), shortNode(from))
		}
	}
	if len(merged) > 0 {
		if err := m.persist(); err != nil {
			utils.Log.Errorf("[cluster] not activating unpersisted received credentials: %v", err)
			return nil
		}
		for _, r := range merged {
			m.enqueueGroup(r.GroupID)
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
			utils.Log.Infof("[cluster] recv revocation group=%s hash=%s v%d origin=%s mount=%s: applied",
				rev.GroupID, shortHash(rev.CredHash), rev.Version, shortNode(rev.Origin), rev.OriginMount)
		}
	}
	if len(merged) > 0 {
		if err := m.persist(); err != nil {
			utils.Log.Errorf("[cluster] not relaying unpersisted revocations: %v", err)
			return nil
		}
		for _, rev := range merged {
			m.enqueueGroup(rev.GroupID)
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
	reply := &syncMessage{Type: "reply", Proto: protoLevel}

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
	// Revocations ride only when the peer asked for them (digest mismatch) or it
	// is a protocol-1 node, whose announces carry its full set and which expects
	// ours back whenever the two differ.
	switch {
	case in.WantRevs:
		reply.Revocations = m.state.revocationSnapshot()
	case in.Proto < protoLevel && in.Type == "pull":
		reply.Revocations = m.state.revocationSnapshot()
	case in.Proto < protoLevel && (in.Type == "hello" || in.Type == "announce"):
		if revocationDigest(in.Revocations) != m.state.revocationDigest() {
			reply.Revocations = m.state.revocationSnapshot()
		}
	}

	if reply.Groups == nil && len(reply.Creds) == 0 && len(reply.Revocations) == 0 && len(reply.Wants) == 0 {
		return nil
	}
	return reply
}

// ---- networking ----

// / seal wraps a message in an encrypted envelope addressed from this node. The
// protocol level is stamped on a copy, so shared messages are never mutated.
func (m *Manager) seal(msg *syncMessage) ([]byte, error) {
	key, err := deriveAEADKey([]byte(m.cfgStore.get().Key))
	if err != nil {
		return nil, err
	}
	cp := *msg
	if cp.Proto == 0 {
		cp.Proto = protoLevel
	}
	return sealEnvelope(key, m.id, &cp, now())
}

// sendTo queues a message on a single connection. A link that was just closed as
// a duplicate hands the message to the surviving link to the same node.
func (m *Manager) sendTo(c *peerConn, msg *syncMessage) {
	if c.isClosed() {
		if id := c.node(); id != "" {
			if alt := m.conns.get(id); alt != nil && alt != c {
				alt.enqueue(msg)
			}
		}
		return
	}
	c.enqueue(msg)
}

// broadcast queues a message on every live connection. Each link coalesces it
// into its own pending state; nothing here can block or drop a connection.
func (m *Manager) broadcast(msg *syncMessage) {
	for _, c := range m.conns.all() {
		c.enqueue(msg)
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
	for _, c := range conns {
		if c == except {
			continue
		}
		c.enqueue(msg)
	}
}

// bindConn records the authenticated node id of a link and, when a second link
// to the same node exists, keeps exactly one of the two (see keepNewLink). What
// the closed link still had queued moves to the survivor.
func (m *Manager) bindConn(c *peerConn, nodeID string) {
	loser := m.conns.bind(c, nodeID, m.id.NodeID)
	if c.outbound {
		m.noteDialNode(c.addr, nodeID)
	}
	if loser == nil {
		m.links.connected(c, nodeID)
		return
	}
	if winner := m.conns.get(nodeID); winner != nil && winner != loser {
		winner.out.absorb(loser.out)
	}
	utils.Log.Infof("[cluster] duplicate link to %s: closing the %s link, keeping the other", shortNode(nodeID), dirOf(loser))
	loser.close(reasonSuperseded)
	if loser != c {
		m.links.connected(c, nodeID)
	}
}

// handleFrame authenticates and processes one inbound frame. A frame that fails
// to open (wrong key / replay / stale) drops the connection.
func (m *Manager) handleFrame(c *peerConn, data []byte) {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		c.close(reasonInactive)
		return
	}
	key, err := deriveAEADKey([]byte(cfg.Key))
	if err != nil {
		c.close(reasonRejected)
		return
	}
	env, msg, err := openEnvelope(key, data, now(), m.replay, replayWindowSec)
	if err != nil {
		utils.Log.Debugf("[cluster] frame rejected from %s: %v", shortNode(c.node()), err)
		c.close(reasonRejected + ": " + shortErr(err))
		return
	}
	if env.Sender == m.id.NodeID {
		c.close(reasonSelf) // connected to ourselves
		return
	}
	if c.node() == "" {
		m.bindConn(c, env.Sender)
	}
	proto := int32(msg.Proto)
	if proto == 0 {
		proto = 1 // a node that predates protocol 2 sends no level
	}
	c.peerProto.Store(proto)
	m.links.noteProto(env.Sender, int(proto))
	from := env.Sender

	// A directed message for someone else is forwarded, never processed.
	if msg.To != "" && msg.To != m.id.NodeID {
		m.forwardDirected(msg, c)
		return
	}
	switch msg.Type {
	case "hello", "announce", "reply":
		c.lastAE.Store(time.Now().UnixNano())
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
	// Refresher leases and the admin's preferred-refresher policy (protocol 2).
	if leases := m.absorbLeases(msg.Leases); len(leases) > 0 {
		relay.Leases = leases
		relayHas = true
	}
	if pols := m.absorbPolicies(msg.Policies); len(pols) > 0 {
		relay.Policies = pols
		relayHas = true
	}
	// Candidate tombstones must converge before credentials: otherwise a relay
	// can briefly resurrect an old signed token while a revocation is in flight.
	if revoked := m.absorbRevocations(msg.Revocations); len(revoked) > 0 {
		relay.Revocations = revoked
		relayHas = true
	}
	// Announces carry a revocation digest instead of the set: when it differs
	// from ours, fetch the peer's set once per distinct (theirs, ours) pair.
	if msg.Proto >= 2 && (msg.Type == "hello" || msg.Type == "announce") {
		mine := m.state.revocationDigest()
		if msg.RevDigest != mine {
			if k := msg.RevDigest + "|" + mine; c.lastRevPull.Load() != k {
				c.lastRevPull.Store(k)
				utils.Log.Infof("[cluster] revocation digest differs from %s (theirs %q ours %q): pulling their set", shortNode(from), msg.RevDigest, mine)
				m.sendTo(c, &syncMessage{Type: "pull", WantRevs: true, Proto: protoLevel})
			}
		}
	}
	// Credentials.
	if merged := m.absorbCredsFrom(msg.Creds, from); len(merged) > 0 {
		m.ackMerged(c, merged)
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
	// Delivery confirmations: explicit acks, and the digests of every announce.
	for _, dg := range msg.Acks {
		ackFrom := msg.From
		if ackFrom == "" {
			ackFrom = from
		}
		m.confirmDelivery(ackFrom, dg)
	}
	if msg.Type == "hello" || msg.Type == "announce" {
		for _, dg := range msg.CredDigests {
			m.confirmDelivery(from, dg)
		}
	}
	if msg.Type == msgRefreshRequest {
		reqFrom := msg.From
		if reqFrom == "" {
			reqFrom = from
		}
		m.handleRefreshRequest(reqFrom, msg.Req)
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
