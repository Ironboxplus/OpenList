package cluster

import (
	"math/rand"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// nextBackoff doubles a redial backoff from dialBackoffMin up to dialBackoffMax.
func nextBackoff(cur time.Duration) time.Duration {
	if cur < dialBackoffMin {
		return dialBackoffMin
	}
	next := cur * 2
	if next > dialBackoffMax {
		next = dialBackoffMax
	}
	return next
}

// jitterDuration spreads d by +-25% so a restarted hub is not hit by every
// peer's redial in the same instant.
func jitterDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	span := int64(d / 2)
	if span <= 0 {
		return d
	}
	return d - d/4 + time.Duration(rand.Int63n(span))
}

// linkStat is what we remember about the link to one peer across reconnects:
// the data the admin needs to tell a flapping or silent link from a healthy one.
type linkStat struct {
	NodeID           string
	Connects         int
	Disconnects      int
	LastReason       string
	LastDisconnectAt int64
	LastConnectedAt  int64
	LastLived        time.Duration
	Proto            int
	// recent holds unix seconds of the latest disconnects (for the flap count).
	recent []int64
}

const maxRecentDisconnects = 32

// linkBook keeps per-peer link history.
type linkBook struct {
	mu    sync.Mutex
	nodes map[string]*linkStat
}

func (b *linkBook) statLocked(nodeID string) *linkStat {
	if b.nodes == nil {
		b.nodes = make(map[string]*linkStat)
	}
	s := b.nodes[nodeID]
	if s == nil {
		s = &linkStat{NodeID: nodeID}
		b.nodes[nodeID] = s
	}
	return s
}

func (b *linkBook) connected(c *peerConn, nodeID string) {
	b.mu.Lock()
	s := b.statLocked(nodeID)
	s.Connects++
	s.LastConnectedAt = now()
	b.mu.Unlock()
	dir := "in"
	if c.outbound {
		dir = "out"
	}
	utils.Log.Infof("[cluster] link up node=%s dir=%s addr=%s", shortNode(nodeID), dir, c.addr)
}

func (b *linkBook) disconnected(c *peerConn, reason string) {
	id := c.node()
	lived := time.Since(time.Unix(c.since, 0))
	if id == "" {
		utils.Log.Infof("[cluster] link down (unauthenticated peer) dir=%s addr=%s reason=%s lived=%s",
			dirOf(c), c.addr, reason, lived.Round(time.Second))
		return
	}
	if reason == reasonSuperseded {
		// Closing the loser of a duplicate-link tie-break says nothing about the
		// pair's link quality; the surviving link carries on.
		utils.Log.Infof("[cluster] link closed node=%s dir=%s reason=%s", shortNode(id), dirOf(c), reason)
		return
	}
	b.mu.Lock()
	s := b.statLocked(id)
	s.Disconnects++
	s.LastReason = reason
	s.LastDisconnectAt = now()
	s.LastLived = lived
	s.recent = append(s.recent, now())
	if len(s.recent) > maxRecentDisconnects {
		s.recent = s.recent[len(s.recent)-maxRecentDisconnects:]
	}
	b.mu.Unlock()
	utils.Log.Infof("[cluster] link down node=%s dir=%s reason=%s lived=%s queue=%d",
		shortNode(id), dirOf(c), reason, lived.Round(time.Second), c.queueDepth())
}

func dirOf(c *peerConn) string {
	if c.outbound {
		return "out"
	}
	return "in"
}

// snapshot returns a copy of a node's stat and its disconnect count in the last
// window seconds.
func (b *linkBook) snapshot(nodeID string, window int64) (linkStat, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nodes == nil {
		b.nodes = make(map[string]*linkStat)
	}
	s := b.nodes[nodeID]
	if s == nil {
		return linkStat{NodeID: nodeID}, 0
	}
	cp := *s
	cp.recent = nil
	n := 0
	cut := now() - window
	for _, t := range s.recent {
		if t >= cut {
			n++
		}
	}
	return cp, n
}

func (b *linkBook) nodeIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.nodes))
	for id := range b.nodes {
		out = append(out, id)
	}
	return out
}

// peerProtoOf is the protocol level last seen from a node (0 = never heard).
func (m *Manager) peerProtoOf(nodeID string) int {
	if nodeID == m.id.NodeID {
		return protoLevel
	}
	if m.conns != nil {
		if c := m.conns.get(nodeID); c != nil {
			if p := c.peerProto.Load(); p > 0 {
				return int(p)
			}
		}
	}
	m.links.mu.Lock()
	defer m.links.mu.Unlock()
	if s := m.links.nodes[nodeID]; s != nil {
		return s.Proto
	}
	return 0
}

// noteProto remembers a peer's protocol level across reconnects.
func (b *linkBook) noteProto(nodeID string, proto int) {
	b.mu.Lock()
	s := b.statLocked(nodeID)
	s.Proto = proto
	b.mu.Unlock()
}
