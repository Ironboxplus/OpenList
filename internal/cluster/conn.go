package cluster

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/gorilla/websocket"
)

// Transport model (NAT-friendly):
//
// The sync link is a STATEFUL, persistent WebSocket, never a stateless
// per-message HTTP request. A node behind NAT cannot be dialed, so it instead
// dials OUT to the reachable peers listed in its config and keeps that
// connection open; frames flow in both directions over it for the connection's
// lifetime. A reachable node also ACCEPTS inbound connections at
// /api/cluster/ws and RELAYS applied records between everyone it is connected
// to — so two NAT'd nodes that both dial the same reachable peer still converge
// through it. Every frame is the same sealed envelope used elsewhere, so the
// crypto/CRDT layers are unchanged and transport-agnostic.
const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	dialBackoffMin = 2 * time.Second
	dialBackoffMax = 60 * time.Second
	sendQueueLen   = 128
	maxFrameBytes  = 16 << 20
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Auth is the cluster PSK (AEAD), not the HTTP origin — accept any origin.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// peerConn is one authenticated, persistent connection to another node.
type peerConn struct {
	mgr       *Manager
	ws        *websocket.Conn
	send      chan []byte
	outbound  bool   // true if we dialed it
	addr      string // base URL we dialed (outbound only); "" for inbound
	since     int64  // unix seconds the connection was established
	nodeID    string // learned from the first authenticated frame
	closeOnce sync.Once
	closed    chan struct{}
}

func newPeerConn(mgr *Manager, ws *websocket.Conn, outbound bool, addr string) *peerConn {
	return &peerConn{
		mgr:      mgr,
		ws:       ws,
		send:     make(chan []byte, sendQueueLen),
		outbound: outbound,
		addr:     addr,
		since:    now(),
		closed:   make(chan struct{}),
	}
}

// enqueue queues a frame for sending; drops it (and closes the conn) if the peer
// is too slow, so a stuck peer can't block the whole node.
func (c *peerConn) enqueue(frame []byte) {
	select {
	case c.send <- frame:
	case <-c.closed:
	default:
		utils.Log.Warnf("[cluster] peer %s send queue full, dropping connection", c.nodeID)
		c.close()
	}
}

func (c *peerConn) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.ws.Close()
		c.mgr.conns.remove(c)
	})
}

// readPump authenticates and dispatches every incoming frame for the connection's
// lifetime.
func (c *peerConn) readPump() {
	defer c.close()
	c.ws.SetReadLimit(maxFrameBytes)
	_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		c.mgr.handleFrame(c, data)
	}
}

// writePump serializes all writes for the connection and keeps it alive with
// periodic pings (essential to hold a NAT mapping open).
func (c *peerConn) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.close()
	}()
	for {
		select {
		case <-c.closed:
			return
		case frame := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// connRegistry tracks all live connections and indexes the most recent one per
// node id.
type connRegistry struct {
	mu     sync.RWMutex
	conns  map[*peerConn]struct{}
	byNode map[string]*peerConn
}

func newConnRegistry() *connRegistry {
	return &connRegistry{
		conns:  make(map[*peerConn]struct{}),
		byNode: make(map[string]*peerConn),
	}
}

func (r *connRegistry) add(c *peerConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[c] = struct{}{}
}

// bind records the authenticated node id for a connection (idempotent).
func (r *connRegistry) bind(c *peerConn, nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.nodeID = nodeID
	r.byNode[nodeID] = c
}

func (r *connRegistry) remove(c *peerConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, c)
	if c.nodeID != "" && r.byNode[c.nodeID] == c {
		delete(r.byNode, c.nodeID)
	}
}

// all returns a snapshot of live connections.
func (r *connRegistry) all() []*peerConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*peerConn, 0, len(r.conns))
	for c := range r.conns {
		out = append(out, c)
	}
	return out
}

// hasNode reports whether we already have a live connection to a node id (used
// to avoid duplicate dialed+accepted links to the same peer).
func (r *connRegistry) hasNode(nodeID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byNode[nodeID]
	return ok
}

func (r *connRegistry) closeAll() {
	for _, c := range r.all() {
		c.close()
	}
}

// ---- connection establishment ----

// wsURL converts a peer base URL (http/https) into its cluster WebSocket URL.
func wsURL(peer string) string {
	peer = strings.TrimRight(strings.TrimSpace(peer), "/")
	switch {
	case strings.HasPrefix(peer, "https://"):
		peer = "wss://" + strings.TrimPrefix(peer, "https://")
	case strings.HasPrefix(peer, "http://"):
		peer = "ws://" + strings.TrimPrefix(peer, "http://")
	case strings.HasPrefix(peer, "wss://"), strings.HasPrefix(peer, "ws://"):
		// already a ws URL
	default:
		peer = "ws://" + peer
	}
	return peer + wsPath
}

// startConn registers a connection, starts its pumps, and greets the peer with a
// hello (our inventory + known peer addresses for PEX + groups doc + credential
// digests) so discovery and anti-entropy begin immediately.
func (m *Manager) startConn(c *peerConn) {
	m.conns.add(c)
	go c.writePump()
	go c.readPump()
	m.sendTo(c, m.helloMessage())
}

// dialPeer opens an outbound persistent connection to a peer and blocks until it
// closes. Dialing OUT is what lets a NAT'd node participate without being
// reachable itself.
func (m *Manager) dialPeer(addr string) error {
	ws, _, err := websocket.DefaultDialer.Dial(wsURL(addr), nil)
	if err != nil {
		return err
	}
	c := newPeerConn(m, ws, true, addr)
	m.startConn(c)
	<-c.closed
	return nil
}

// ServeWS upgrades an inbound HTTP request to a persistent connection. Auth is
// deferred to the first sealed frame (PSK), so the upgrade itself is open. Any
// node that can open our AEAD envelope is admitted as a peer — this is the
// "authenticated remotes auto-join" half of discovery.
func (m *Manager) ServeWS(w http.ResponseWriter, r *http.Request) {
	cfg := m.cfgStore.get()
	if !cfg.active() {
		http.Error(w, "cluster sync disabled", http.StatusServiceUnavailable)
		return
	}
	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	m.startConn(newPeerConn(m, ws, false, ""))
}

// dialSupervisor keeps live outbound connections to bootstrap seeds and to every
// auto-discovered peer address we are not already connected to, re-dialing as
// connections drop. Seeds bootstrap the first join; learned addresses (from peer
// inventory exchange) keep public nodes meshed without manual config.
func (m *Manager) dialSupervisor() {
	for {
		if cfg := m.cfgStore.get(); cfg.active() {
			for _, seed := range cfg.seedList() {
				m.ensureDial(seed)
			}
			for _, addr := range m.discoveredDialTargets() {
				m.ensureDial(addr)
			}
		}
		select {
		case <-m.stopCh:
			return
		case <-time.After(dialReconcile):
		}
	}
}

// discoveredDialTargets returns advertised peer addresses we should dial: those
// belonging to nodes we do not already have a live connection to, and that are
// not our own advertised address.
func (m *Manager) discoveredDialTargets() []string {
	self := m.cfgStore.get().Addr
	self = trimURL(self)
	var out []string
	for _, n := range m.state.inventoryList() {
		if n.NodeID == m.id.NodeID || n.Addr == "" {
			continue
		}
		if trimURL(n.Addr) == self && self != "" {
			continue
		}
		if m.conns.hasNode(n.NodeID) {
			continue
		}
		out = append(out, n.Addr)
	}
	return out
}

func trimURL(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// ensureDial starts (at most one) outbound dial loop for a peer URL. dialPeer
// blocks for the connection's lifetime, so the dialing flag also prevents a
// duplicate link while connected.
func (m *Manager) ensureDial(addr string) {
	addr = trimURL(addr)
	if addr == "" {
		return
	}
	m.dialMu.Lock()
	if m.dialing[addr] {
		m.dialMu.Unlock()
		return
	}
	m.dialing[addr] = true
	m.dialMu.Unlock()
	go func() {
		defer func() {
			m.dialMu.Lock()
			delete(m.dialing, addr)
			m.dialMu.Unlock()
		}()
		if err := m.dialPeer(addr); err != nil {
			utils.Log.Debugf("[cluster] dial %s failed: %v", addr, err)
			select {
			case <-m.stopCh:
			case <-time.After(dialBackoffMin):
			}
		}
	}()
}
