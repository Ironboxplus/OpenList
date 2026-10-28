package cluster

import (
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// ----------------------------------------------------------------------------
// Delivery confirmation
//
// A new credential version is pushed once; if that push is lost (link flapping,
// a relay that was down) the group only converges at the next announce. To close
// that gap the author tracks who has confirmed the version. A member confirms by
// (a) the immediate ack it sends after merging the record and (b) the credential
// digests in its announce/hello. A member that stays silent past the resend
// delay gets the record again, addressed directly (relayed by a hub if there is
// no direct link), up to maxDeliveryAttempts times.
// ----------------------------------------------------------------------------

const (
	maxDeliveryAttempts = 5
	deliveryExpiry      = 10 * time.Minute
)

type delivery struct {
	rec       *credRecord
	created   time.Time
	lastSend  time.Time
	attempts  int
	confirmed map[string]bool
}

type deliveryBook struct {
	mu sync.Mutex
	by map[string]*delivery // credDigestKey -> newest tracked version
}

// trackDelivery starts tracking a record this node just authored and pushed.
func (m *Manager) trackDelivery(r *credRecord) {
	if r == nil {
		return
	}
	cp := *r
	m.deliveries.mu.Lock()
	defer m.deliveries.mu.Unlock()
	if m.deliveries.by == nil {
		m.deliveries.by = make(map[string]*delivery)
	}
	m.deliveries.by[credDigestKey(digestOfCred(r))] = &delivery{
		rec: &cp, created: time.Now(), lastSend: time.Now(), confirmed: map[string]bool{},
	}
}

// confirmDelivery notes that node holds at least the tracked version.
func (m *Manager) confirmDelivery(node string, dg credDigest) {
	m.deliveries.mu.Lock()
	defer m.deliveries.mu.Unlock()
	d := m.deliveries.by[credDigestKey(dg)]
	if d == nil {
		return
	}
	if dg.Version >= d.rec.Version || dg.CredHash == d.rec.CredHash {
		if !d.confirmed[node] {
			d.confirmed[node] = true
			utils.Log.Debugf("[cluster] delivery confirmed group=%s v%d by %s after %s", d.rec.GroupID, d.rec.Version, shortNode(node), time.Since(d.created).Round(time.Millisecond))
		}
	}
}

// deliveryStatus is for tests and the status view: the nodes that confirmed.
func (m *Manager) deliveryConfirmed(key string) (map[string]bool, bool) {
	m.deliveries.mu.Lock()
	defer m.deliveries.mu.Unlock()
	d := m.deliveries.by[key]
	if d == nil {
		return nil, false
	}
	out := make(map[string]bool, len(d.confirmed))
	for k, v := range d.confirmed {
		out[k] = v
	}
	return out, true
}

// deliveryTick resends to unconfirmed online members and retires finished or
// expired entries.
func (m *Manager) deliveryTick() {
	if m.cfgStore == nil || m.state == nil {
		return
	}
	resend := m.cfgStore.get().deliveryResend()
	type job struct {
		node string
		rec  *credRecord
		n    int
	}
	var jobs []job
	m.deliveries.mu.Lock()
	for key, d := range m.deliveries.by {
		if time.Since(d.created) > deliveryExpiry {
			delete(m.deliveries.by, key)
			continue
		}
		g, ok := m.state.groupByID(d.rec.GroupID)
		if !ok {
			delete(m.deliveries.by, key)
			continue
		}
		var missing []string
		seen := map[string]bool{}
		for _, mem := range g.Members {
			if mem.NodeID == m.id.NodeID || seen[mem.NodeID] {
				continue
			}
			seen[mem.NodeID] = true
			if d.confirmed[mem.NodeID] {
				continue
			}
			if n, ok := m.nodeInfoOf(mem.NodeID); !ok || !m.isOnline(mem.NodeID, n.seenAt) {
				continue
			}
			missing = append(missing, mem.NodeID)
		}
		if len(missing) == 0 {
			delete(m.deliveries.by, key)
			continue
		}
		if time.Since(d.lastSend) < resend || d.attempts >= maxDeliveryAttempts {
			continue
		}
		d.attempts++
		d.lastSend = time.Now()
		for _, node := range missing {
			jobs = append(jobs, job{node, d.rec, d.attempts})
		}
	}
	m.deliveries.mu.Unlock()
	for _, j := range jobs {
		utils.Log.Infof("[cluster] resending group=%s v%d to unconfirmed member %s (attempt %d)", j.rec.GroupID, j.rec.Version, shortNode(j.node), j.n)
		m.sendDirected(j.node, &syncMessage{Type: "push", Creds: []*credRecord{j.rec}})
	}
}

func (m *Manager) nodeInfoOf(nodeID string) (nodeInfo, bool) {
	for _, n := range m.state.inventoryList() {
		if n.NodeID == nodeID {
			return n, true
		}
	}
	return nodeInfo{}, false
}

// ackMerged tells each origin which of its records we just merged.
func (m *Manager) ackMerged(c *peerConn, recs []*credRecord) {
	byOrigin := map[string][]credDigest{}
	for _, r := range recs {
		if r.Origin != m.id.NodeID {
			byOrigin[r.Origin] = append(byOrigin[r.Origin], digestOfCred(r))
		}
	}
	for origin, dgs := range byOrigin {
		msg := &syncMessage{Type: msgAck, Acks: dgs, From: m.id.NodeID, To: origin, Proto: protoLevel}
		if direct := m.conns.get(origin); direct != nil {
			direct.enqueue(msg)
		} else if c != nil {
			c.enqueue(msg) // the link it came over leads towards the origin
		}
	}
}
