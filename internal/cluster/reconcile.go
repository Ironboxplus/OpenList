package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"k8s.io/client-go/util/workqueue"
)

// Credential recovery is level-triggered. The credential catalogue (state.creds)
// is the desired state, the pair a local mount is running is the actual state,
// and reconcile is the only code that moves the second towards the first.
//
// Events (a push, a local 401, a storage becoming registered, a group change)
// do nothing but wake a key. A key is retried until its mount agrees with the
// catalogue, and every key is woken again each resync period, so an event whose
// preconditions were not met when it fired (the storage was not registered yet,
// a frame was lost, the node was stopped) costs a delay and never the recovery.
//
// The queue is client-go's workqueue: one key is processed by one worker at a
// time, an Add during processing is replayed after Done, and failed keys back
// off per key.

const reconcileKeySep = "\x00"

func reconcileKey(groupID, mount string) string { return groupID + reconcileKeySep + mount }

func splitReconcileKey(key string) (groupID, mount string, ok bool) {
	groupID, mount, ok = strings.Cut(key, reconcileKeySep)
	return groupID, mount, ok && groupID != "" && mount != ""
}

// reconcileQueue returns the queue, creating it and its single worker on first
// use. Different mounts are processed one after another: 115 does not allow
// concurrent use of a credential anyway.
func (m *Manager) reconcileQueue() workqueue.TypedRateLimitingInterface[string] {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	if m.queue == nil {
		m.queue = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
		go m.reconcileWorker(m.queue)
	}
	return m.queue
}

func (m *Manager) reconcileWorker(q workqueue.TypedRateLimitingInterface[string]) {
	for {
		key, shutdown := q.Get()
		if shutdown {
			return
		}
		m.reconcileSafely(q, key)
		q.Done(key)
	}
}

func (m *Manager) reconcileSafely(q workqueue.TypedRateLimitingInterface[string], key string) {
	defer func() {
		if r := recover(); r != nil {
			utils.Log.Errorf("[cluster] reconcile %q panicked: %v", key, r)
			q.AddRateLimited(key)
		}
	}()
	m.reconcile(q, key)
}

// ---- triggers: they only enqueue ----

// enqueueMount wakes every group this node's mount belongs to.
func (m *Manager) enqueueMount(mount string) {
	for _, g := range m.state.groupsForMount(m.id.NodeID, mount) {
		m.reconcileQueue().Add(reconcileKey(g.ID, mount))
	}
}

// enqueueGroup wakes this node's mounts in one group.
func (m *Manager) enqueueGroup(groupID string) {
	g, ok := m.state.groupByID(groupID)
	if !ok {
		return
	}
	for _, mount := range g.mountsForNode(m.id.NodeID) {
		m.reconcileQueue().Add(reconcileKey(groupID, mount))
	}
}

// enqueueAll wakes every member mount of this node.
func (m *Manager) enqueueAll() {
	for _, g := range m.state.groupList() {
		for _, mount := range g.mountsForNode(m.id.NodeID) {
			m.reconcileQueue().Add(reconcileKey(g.ID, mount))
		}
	}
}

// resyncLoop wakes everything periodically (client-go's shared informer resync):
// the safety net under every trigger above.
func (m *Manager) resyncLoop() {
	for {
		select {
		case <-m.stopCh:
			return
		case <-time.After(m.cfgStore.get().resyncInterval()):
		}
		m.enqueueAll()
	}
}

// ---- the reconcile ----

func (m *Manager) reconcile(q workqueue.TypedRateLimitingInterface[string], key string) {
	groupID, mount, ok := splitReconcileKey(key)
	if !ok {
		q.Forget(key)
		return
	}
	cfg := m.cfgStore.get()
	if !cfg.active() || !cfg.ApplyRemote || !m.mountInGroup(groupID, mount) {
		q.Forget(key)
		return
	}

	// One adoption at a time per mount, shared with nothing else that writes it.
	lock := m.recoveryLock(mount)
	lock.Lock()
	defer lock.Unlock()

	d, err := op.GetStorageByMountPath(mount)
	if err != nil {
		// Not registered yet (storage Init is still running, e.g. the 401 that
		// started all this) or disabled. Never drop the key; the storage hook
		// wakes it as well, this is the floor.
		q.AddRateLimited(key)
		return
	}
	before := *d.GetStorage() // also the rollback point

	if _, ok := m.state.credByHash(groupID, credHash(extractCreds(before.Addition))); ok {
		m.forgetSaved(mount) // catalogued now: the version rule decides
	}
	want := m.desiredCandidate(groupID, mount, before)
	if want == nil {
		if before.Status == op.WORK {
			q.Forget(key)
			return
		}
		// Nothing in the catalogue can fix a failed mount: ask the peers and look
		// again later. Whether to ask the refresher is refresher.go's call.
		m.pullGroupCooled(groupID)
		q.AddAfter(key, cfg.resyncInterval())
		return
	}

	newAdd, _ := applyCreds(before.Addition, want.Payload)
	probe := m.probeCredentialFn
	if probe == nil {
		probe = probeCredential
	}
	// Candidate payloads are verified by a temporary real driver before the
	// production storage is touched.
	if err := probe(context.Background(), before, newAdd); err != nil {
		utils.Log.Warnf("[cluster] probe candidate %s for %s failed: %v", shortHash(want.CredHash), mount, err)
		switch {
		case isProviderRefreshThrottle(err):
			// 40140117: a provider cooldown. A fresh probe now would only extend it.
			q.AddAfter(key, m.retryDelay())
		case isProvider401(err):
			if m.rejectCandidate(groupID, mount, want) {
				q.Add(key) // next candidate
			} else {
				q.AddRateLimited(key)
			}
		default:
			q.AddRateLimited(key)
		}
		return
	}

	st := before
	st.Addition = newAdd
	if err := op.UpdateStorage(context.Background(), st); err != nil {
		utils.Log.Warnf("[cluster] apply creds to %s failed: %v", mount, err)
		// UpdateStorage persists before it initializes. Restore the complete
		// previous storage record rather than leaving an unproven pair behind.
		if rollbackErr := op.UpdateStorage(context.Background(), before); rollbackErr != nil {
			utils.Log.Errorf("[cluster] rollback %s after failed candidate commit: %v", mount, rollbackErr)
		}
		q.AddRateLimited(key)
		return
	}
	m.state.markCandidateHealthy(groupID, want.CredHash, now())
	m.recordEvent("apply", groupID, fmt.Sprintf("%s adopted credentials from %s", mount, shortNode(want.Origin)))
	utils.Log.Infof("[cluster] applied group %s credentials to %s (v%d from %s)", groupID, mount, want.Version, want.Origin)
	q.Forget(key)
}

func (m *Manager) mountInGroup(groupID, mount string) bool {
	g, ok := m.state.groupByID(groupID)
	if !ok {
		return false
	}
	for _, mp := range g.mountsForNode(m.id.NodeID) {
		if mp == mount {
			return true
		}
	}
	return false
}

// savedPair is what the node knew when a mount's storage was about to be saved
// with a pair the catalogue had no record of: a local rotation (the driver saves
// the new pair before onStorageCredential catalogues it) or an administrator's
// edit. It is persisted (saved_pairs.json) because a restart between the save
// and the cataloguing leaves the same uncatalogued pair behind. The
// pair was made after the records in known, so none of them is newer than it.
// Records absorbed later are not in known and compete normally.
type savedPair struct {
	hash  string
	known map[string]struct{} // group, separator, credential hash
}

// onStoragePreSave runs just before op writes a storage record. It is the only
// point that precedes both the database write and the asynchronous "update"
// hook, so a crash or a resync after the write cannot find the pair without its
// snapshot.
func (m *Manager) onStoragePreSave(st model.Storage) {
	select {
	case <-m.stopCh:
		return
	default:
	}
	if !m.cfgStore.get().active() {
		return
	}
	m.notePairSaved(&st)
}

// notePairSaved remembers which catalogued records the pair about to be saved is
// ahead of, and persists that before the save so a restart keeps it. Only hashes
// are stored, never credentials. A pair that is already catalogued needs no
// snapshot: its version decides.
func (m *Manager) notePairSaved(st *model.Storage) {
	hash := credHash(extractCreds(st.Addition))
	known := map[string]struct{}{}
	catalogued := false
	for _, g := range m.state.groupsForMount(m.id.NodeID, st.MountPath) {
		if _, ok := m.state.credByHash(g.ID, hash); ok {
			catalogued = true
		}
		for _, r := range m.state.credsForGroup(g.ID) {
			known[g.ID+reconcileKeySep+r.CredHash] = struct{}{}
		}
	}
	m.savedMu.Lock()
	defer m.savedMu.Unlock()
	if m.saved == nil {
		m.saved = make(map[string]savedPair)
	}
	if hash == "" || catalogued || len(known) == 0 {
		m.dropSavedLocked(st.MountPath)
		return
	}
	m.saved[st.MountPath] = savedPair{hash: hash, known: known}
	m.persistSavedLocked()
}

// forgetSaved drops a mount's snapshot (storage deleted, or its pair is
// catalogued and the version rule applies).
func (m *Manager) forgetSaved(mount string) {
	m.savedMu.Lock()
	defer m.savedMu.Unlock()
	m.dropSavedLocked(mount)
}

func (m *Manager) dropSavedLocked(mount string) {
	if _, ok := m.saved[mount]; !ok {
		return
	}
	delete(m.saved, mount)
	m.persistSavedLocked()
}

func (m *Manager) savedPath() string { return filepath.Join(m.dir, "saved_pairs.json") }

type savedPairFile struct {
	Hash  string   `json:"hash"`
	Known []string `json:"known"`
}

func (m *Manager) loadSaved() {
	b, err := os.ReadFile(m.savedPath())
	if err != nil {
		return
	}
	var in map[string]savedPairFile
	if err := json.Unmarshal(b, &in); err != nil {
		utils.Log.Warnf("[cluster] corrupt saved-pair file, ignoring: %v", err)
		return
	}
	m.savedMu.Lock()
	defer m.savedMu.Unlock()
	m.saved = make(map[string]savedPair, len(in))
	for mount, f := range in {
		sp := savedPair{hash: f.Hash, known: make(map[string]struct{}, len(f.Known))}
		for _, k := range f.Known {
			sp.known[k] = struct{}{}
		}
		m.saved[mount] = sp
	}
}

// persistSavedLocked writes the snapshots atomically: temp file, fsync, rename.
func (m *Manager) persistSavedLocked() {
	if m.diskClosed {
		return
	}
	out := make(map[string]savedPairFile, len(m.saved))
	for mount, sp := range m.saved {
		f := savedPairFile{Hash: sp.hash, Known: make([]string, 0, len(sp.known))}
		for k := range sp.known {
			f.Known = append(f.Known, k)
		}
		sort.Strings(f.Known)
		out[mount] = f
	}
	b, err := json.Marshal(out)
	if err == nil {
		err = os.MkdirAll(m.dir, 0o700)
	}
	if err == nil {
		err = writeFileSynced(m.savedPath(), b)
	}
	if err != nil {
		utils.Log.Errorf("[cluster] failed to persist saved-pair snapshots: %v", err)
	}
}

func writeFileSynced(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// aheadOfRunningPair reports whether the record predates the uncatalogued pair
// the mount is running, so adopting it would move the mount backwards.
func (m *Manager) aheadOfRunningPair(groupID, mount, currentHash, recHash string) bool {
	m.savedMu.Lock()
	defer m.savedMu.Unlock()
	sp, ok := m.saved[mount]
	if !ok || sp.hash != currentHash {
		return false
	}
	_, older := sp.known[groupID+reconcileKeySep+recHash]
	return older
}

// desiredCandidate picks the record this mount should be running: the highest
// version among the catalogued candidates that are allowed on it. Revoked pairs
// are already absent from the catalogue (revokePair and mergeRevocation delete
// them, mergeCred refuses them). The rules are those the event-driven recovery
// applied one candidate at a time:
//   - not authorized, quarantined on this mount, or for another driver: skip;
//   - already the mount's credentials (applyCreds reports no change): skip;
//   - a WORK mount is proven only by its own pair, so it never takes a
//     self-authored echo, and never a version not above its current pair's
//     (115 rotates the refresh_token on use: a lower version is a dead pair that
//     still probes clean until its access_token lapses).
func (m *Manager) desiredCandidate(groupID, mount string, st model.Storage) *credRecord {
	var currentVer uint64
	haveCurrent := false
	currentHash := credHash(extractCreds(st.Addition))
	if st.Status == op.WORK {
		if cur, ok := m.state.credByHash(groupID, currentHash); ok {
			currentVer, haveCurrent = cur.Version, true
		}
	}
	var best *credRecord
	for _, r := range m.state.credsForGroup(groupID) {
		if !m.credentialAuthorized(r) || m.state.candidateFailed(groupID, mount, r.CredHash) {
			continue
		}
		if r.OriginDriver != "" && st.Driver != r.OriginDriver {
			continue
		}
		if st.Status == op.WORK && (r.Origin == m.id.NodeID || (haveCurrent && currentVer >= r.Version)) {
			continue
		}
		if st.Status == op.WORK && !haveCurrent && m.aheadOfRunningPair(groupID, mount, currentHash, r.CredHash) {
			continue
		}
		if _, changed := applyCreds(st.Addition, r.Payload); !changed {
			continue
		}
		if best == nil || r.Version > best.Version {
			best = r
		}
	}
	return best
}

// rejectCandidate records a provider 401 on a candidate: tombstone first,
// persisted, and only then broadcast. It reports whether the next candidate may
// be tried now.
func (m *Manager) rejectCandidate(groupID, mount string, r *credRecord) bool {
	rev, revoked := m.state.revokePair(m.id, groupID, mount, r.CredHash, now())
	if !revoked {
		return true // already revoked, so already out of the catalogue
	}
	m.recordEvent("invalidate", groupID,
		fmt.Sprintf("%s rejected candidate %s with provider 401", mount, shortHash(r.CredHash)))
	if err := m.persist(); err != nil {
		utils.Log.Errorf("[cluster] not relaying unpersisted candidate rejection for %s: %v", mount, err)
		return false
	}
	m.broadcast(&syncMessage{Type: "push", Revocations: []*credRevocation{rev}})
	return true
}

func (m *Manager) retryDelay() time.Duration {
	if m.candidateRetryDelay > 0 {
		return m.candidateRetryDelay
	}
	return defaultCandidateRetryDelay
}

// pullGroupCooled asks the peers for a group, at most once per pull wait, so a
// failed mount polled by the resync does not turn into a request storm.
func (m *Manager) pullGroupCooled(groupID string) {
	m.pullMu.Lock()
	if m.lastPull == nil {
		m.lastPull = make(map[string]time.Time)
	}
	if time.Since(m.lastPull[groupID]) < m.pullWait() {
		m.pullMu.Unlock()
		return
	}
	m.lastPull[groupID] = time.Now()
	m.pullMu.Unlock()
	m.pullGroup(groupID)
}
