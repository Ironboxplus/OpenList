package cluster

import (
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
//
// Sending is state-based (see outbox.go): a backlog coalesces, it never closes
// the link. A link ends only on a read/write error, a missed pong, a
// duplicate-link tie-break, or an authentication failure.
const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = 20 * time.Second
	dialBackoffMin = 2 * time.Second
	dialBackoffMax = 60 * time.Second
	// stableLinkSec is how long a link must have lived for its redial backoff to
	// reset; shorter links count as flapping and keep growing the backoff.
	stableLinkSec = 30
	maxFrameBytes = 16 << 20
)

// Disconnect reasons surfaced in link health.
const (
	reasonPeerClosed   = "peer_closed"
	reasonReadTimeout  = "read_timeout"
	reasonReadError    = "read_error"
	reasonWriteTimeout = "write_timeout"
	reasonWriteError   = "write_error"
	reasonRejected     = "frame_rejected"
	reasonSuperseded   = "superseded_duplicate_link"
	reasonSelf         = "connected_to_self"
	reasonConfig       = "config_change"
	reasonShutdown     = "shutdown"
	reasonInactive     = "cluster_inactive"
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Auth is the cluster PSK (AEAD), not the HTTP origin — accept any origin.
	CheckOrigin: func(r *http.Request) bool { return true },
}

var wsDialer = &websocket.Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: 15 * time.Second,
}

// peerConn is one authenticated, persistent connection to another node.
type peerConn struct {
	mgr      *Manager
	ws       *websocket.Conn
	out      *outbox
	outbound bool   // true if we dialed it
	addr     string // base URL we dialed (outbound only); "" for inbound
	since    int64  // unix seconds the connection was established

	idMu   sync.RWMutex
	nodeID string // learned from the first authenticated frame

	closeOnce sync.Once
	closed    chan struct{}
	reason    atomic.Value // string: why the link ended

	rttNanos    atomic.Int64 // last measured ping round trip
	lastRecv    atomic.Int64 // unix nanos of the last frame received
	lastAE      atomic.Int64 // unix nanos of the last hello/announce/reply received
	peerProto   atomic.Int32
	framesSent  atomic.Uint64
	framesRecv  atomic.Uint64
	lastRevPull atomic.Value // string: last (their,our) revocation digest pair we pulled for
}

func newPeerConn(mgr *Manager, ws *websocket.Conn, outbound bool, addr string) *peerConn {
	return &peerConn{
		mgr:      mgr,
		ws:       ws,
		out:      newOutbox(),
		outbound: outbound,
		addr:     addr,
		since:    now(),
		closed:   make(chan struct{}),
	}
}

func (c *peerConn) node() string {
	c.idMu.RLock()
	defer c.idMu.RUnlock()
	return c.nodeID
}

func (c *peerConn) setNode(id string) {
	c.idMu.Lock()
	c.nodeID = id
	c.idMu.Unlock()
}

func (c *peerConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *peerConn) closeReason() string {
	if r, ok := c.reason.Load().(string); ok {
		return r
	}
	return ""
}

// enqueue coalesces a message into the peer's pending state. It never blocks
// and never closes the link: a backlog only means newer state replaces older.
func (c *peerConn) enqueue(msg *syncMessage) {
	if c.out == nil || c.isClosed() {
		return
	}
	c.out.put(msg)
}

func (c *peerConn) queueDepth() int {
	if c.out == nil {
		return 0
	}
	return c.out.depth()
}

func (c *peerConn) close(reason string) {
	c.closeOnce.Do(func() {
		c.reason.Store(reason)
		close(c.closed)
		if c.ws != nil {
			_ = c.ws.Close()
		}
		if c.mgr != nil {
			c.mgr.conns.remove(c)
			c.mgr.links.disconnected(c, reason)
		}
	})
}

// classifyReadErr maps a websocket read error to a disconnect reason.
func classifyReadErr(err error) string {
	var ne net.Error
	switch {
	case websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure):
		return reasonPeerClosed
	case errors.As(err, &ne) && ne.Timeout():
		return reasonReadTimeout
	default:
		return reasonReadError + ": " + shortErr(err)
	}
}

func classifyWriteErr(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return reasonWriteTimeout
	}
	return reasonWriteError + ": " + shortErr(err)
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// readPump authenticates and dispatches every incoming frame for the connection's
// lifetime.
func (c *peerConn) readPump() {
	c.ws.SetReadLimit(maxFrameBytes)
	_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(appData string) error {
		if len(appData) == 8 {
			sent := int64(binary.BigEndian.Uint64([]byte(appData)))
			if rtt := time.Now().UnixNano() - sent; rtt > 0 {
				c.rttNanos.Store(rtt)
			}
		}
		return c.ws.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			c.close(classifyReadErr(err))
			return
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		c.lastRecv.Store(time.Now().UnixNano())
		c.framesRecv.Add(1)
		c.mgr.handleFrame(c, data)
	}
}

// writePump serializes all writes for the connection and keeps it alive with
// periodic pings (essential to hold a NAT mapping open, and the RTT probe).
// Priority state (credentials, revocations, leases, directed messages) is always
// flushed before bulk state.
func (c *peerConn) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		if msg := c.out.takePriority(); msg != nil {
			if !c.writeMsg(msg) {
				return
			}
			continue
		}
		if msg := c.out.takeBulk(); msg != nil {
			if !c.writeMsg(msg) {
				return
			}
			continue
		}
		select {
		case <-c.closed:
			return
		case <-c.out.wake:
		case <-ticker.C:
			var ts [8]byte
			binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixNano()))
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, ts[:]); err != nil {
				c.close(classifyWriteErr(err))
				return
			}
		}
	}
}

// writeMsg seals one message at write time (fresh nonce and timestamp, so a long
// stall can never put a stale envelope on the wire) and writes it. It returns
// false after closing the link on a write error or timeout.
func (c *peerConn) writeMsg(msg *syncMessage) bool {
	frame, err := c.mgr.seal(msg)
	if err != nil {
		utils.Log.Warnf("[cluster] seal %s for %s failed: %v", msg.Type, shortNode(c.node()), err)
		return true
	}
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	if err := c.ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		c.close(classifyWriteErr(err))
		return false
	}
	c.framesSent.Add(1)
	return true
}

// connRegistry tracks all live connections and indexes the current one per node
// id (one link per node pair).
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

// keepNewLink decides, for two live links to the same peer, whether the new one
// replaces the old. Both ends evaluate this with mirrored inputs and reach the
// same verdict, so exactly one physical link survives:
//   - links dialed from opposite sides: keep the one dialed by the node with
//     the lexicographically smaller id;
//   - links dialed from the same side: the newer one wins (the older is most
//     likely a half-dead link the dialer has already given up on).
func keepNewLink(selfID, peerID string, newOutbound, oldOutbound bool) bool {
	dialer := func(out bool) string {
		if out {
			return selfID
		}
		return peerID
	}
	preferred := selfID
	if peerID < preferred {
		preferred = peerID
	}
	dn, do := dialer(newOutbound), dialer(oldOutbound)
	if dn == do {
		return true
	}
	return dn == preferred
}

// bind records the authenticated node id for a connection (idempotent). When a
// second live link to the same node exists it applies keepNewLink and returns
// the link that must be closed (nil if there is no duplicate).
func (r *connRegistry) bind(c *peerConn, nodeID, selfID string) (loser *peerConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.setNode(nodeID)
	old := r.byNode[nodeID]
	if old == nil || old == c || old.isClosed() {
		r.byNode[nodeID] = c
		return nil
	}
	if keepNewLink(selfID, nodeID, c.outbound, old.outbound) {
		r.byNode[nodeID] = c
		return old
	}
	return c
}

func (r *connRegistry) remove(c *peerConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, c)
	if id := c.node(); id != "" && r.byNode[id] == c {
		delete(r.byNode, id)
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

// get returns the current link to a node, if any.
func (r *connRegistry) get(nodeID string) *peerConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byNode[nodeID]
}

// hasNode reports whether we already have a live connection to a node id (used
// to avoid duplicate dialed+accepted links to the same peer).
func (r *connRegistry) hasNode(nodeID string) bool {
	return r.get(nodeID) != nil
}

func (r *connRegistry) closeAll(reason string) {
	for _, c := range r.all() {
		c.close(reason)
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
// reachable itself. It returns how long the link lived.
func (m *Manager) dialPeer(addr string) (time.Duration, error) {
	ws, _, err := wsDialer.Dial(wsURL(addr), nil)
	if err != nil {
		return 0, err
	}
	c := newPeerConn(m, ws, true, addr)
	start := time.Now()
	m.startConn(c)
	<-c.closed
	return time.Since(start), nil
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

// dialState is the redial bookkeeping for one peer URL.
type dialState struct {
	dialing bool
	backoff time.Duration // current backoff (before jitter)
	nextAt  time.Time     // not before
	node    string        // node id learned from a past link, "" if unknown
}

// ensureDial starts (at most one) outbound dial loop for a peer URL. dialPeer
// blocks for the connection's lifetime, so the dialing flag also prevents a
// duplicate link while connected. Failures and short-lived links back off
// exponentially with jitter.
func (m *Manager) ensureDial(addr string) {
	addr = trimURL(addr)
	if addr == "" {
		return
	}
	m.dialMu.Lock()
	if m.dials == nil {
		m.dials = make(map[string]*dialState)
	}
	st := m.dials[addr]
	if st == nil {
		st = &dialState{}
		m.dials[addr] = st
	}
	if st.dialing || time.Now().Before(st.nextAt) {
		m.dialMu.Unlock()
		return
	}
	// A seed URL is not tied to a node until its first link is bound. Once we
	// know the node, never open a second link to it: the existing one is the
	// pair's single link (see keepNewLink).
	if st.node != "" && m.conns.hasNode(st.node) {
		m.dialMu.Unlock()
		return
	}
	st.dialing = true
	m.dialMu.Unlock()
	go func() {
		lived, err := m.dialPeer(addr)
		m.dialMu.Lock()
		st.dialing = false
		if err != nil {
			st.backoff = nextBackoff(st.backoff)
			utils.Log.Debugf("[cluster] dial %s failed (retry in ~%s): %v", addr, st.backoff, err)
		} else if lived < stableLinkSec*time.Second {
			st.backoff = nextBackoff(st.backoff)
		} else {
			st.backoff = 0
		}
		st.nextAt = time.Now().Add(jitterDuration(st.backoff))
		m.dialMu.Unlock()
	}()
}

// noteDialNode remembers which node an outbound address reached.
func (m *Manager) noteDialNode(addr, nodeID string) {
	if addr == "" || nodeID == "" {
		return
	}
	m.dialMu.Lock()
	if st := m.dials[addr]; st != nil {
		st.node = nodeID
	}
	m.dialMu.Unlock()
}

// dialStateFor is for tests/diagnostics.
func (m *Manager) dialStateFor(addr string) (dialing bool, backoff time.Duration) {
	m.dialMu.Lock()
	defer m.dialMu.Unlock()
	if st := m.dials[trimURL(addr)]; st != nil {
		return st.dialing, st.backoff
	}
	return false, 0
}
