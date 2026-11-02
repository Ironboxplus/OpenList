package cluster

import (
	"context"
	"fmt"
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
	if st.Status == op.WORK {
		if cur, ok := m.state.credByHash(groupID, credHash(extractCreds(st.Addition))); ok {
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
