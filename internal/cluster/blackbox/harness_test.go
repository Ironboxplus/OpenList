//go:build blackbox

// Package blackbox runs real OpenList processes (built binaries, separate data
// directories, real sockets) as a cluster whose 115 Open storages talk to the
// emu115 emulator. It is excluded from normal test runs; see docs/cluster-sync.md
// ("Black-box tests") for how to run it.
package blackbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/115-sdk-go/emu115"
)

const adminPass = "bb-admin-pass"

func bbRoot(t *testing.T) string {
	if v := os.Getenv("CLUSTER_BB_DIR"); v != "" {
		return v
	}
	return `E:\Go\Openlist\tmp\cluster-bb`
}

func newBin(t *testing.T) string {
	if v := os.Getenv("CLUSTER_BB_BIN"); v != "" {
		return v
	}
	return filepath.Join(bbRoot(t), "bin-new.exe")
}

func oldBin(t *testing.T) string {
	if v := os.Getenv("CLUSTER_BB_OLD_BIN"); v != "" {
		return v
	}
	return filepath.Join(bbRoot(t), "bin-old.exe")
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// ---- timing knobs given to every node (seconds) ----

type timing struct {
	LeaseTTL, ReqWait, MinGap, Resend, Announce int
}

var fastTiming = timing{LeaseTTL: 6, ReqWait: 4, MinGap: 1, Resend: 3, Announce: 3}

// ---- node process ----

type node struct {
	t       *testing.T
	name    string
	dir     string
	bin     string
	port    int
	cmd     *exec.Cmd
	logf    *os.File
	logPath string
	token   string
	nodeID  string
	killed  bool
}

func (n *node) base() string { return fmt.Sprintf("http://127.0.0.1:%d", n.port) }

type startOpts struct {
	bin    string
	emuURL string
}

func startNode(t *testing.T, root, name string, o startOpts) *node {
	t.Helper()
	dir := filepath.Join(root, name)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	cfg := fmt.Sprintf(`{"scheme":{"address":"127.0.0.1","http_port":%d,"https_port":-1}}`, port)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "stdout.log")
	logf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	n := &node{t: t, name: name, dir: dir, bin: o.bin, port: port, logf: logf, logPath: logPath}
	n.cmd = exec.Command(o.bin, "server", "--data", dir, "--log-std")
	n.cmd.Dir = dir
	n.cmd.Stdout, n.cmd.Stderr = logf, logf
	n.cmd.Env = append(os.Environ(),
		"OPENLIST_ADMIN_PASSWORD="+adminPass,
		"OPENLIST_TEST_115_API_BASE="+o.emuURL,
		"OPENLIST_TEST_115_PASSPORT_BASE="+o.emuURL,
		// Nothing may reach the internet from a test node (frontend fetcher etc.).
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9",
		"NO_PROXY=127.0.0.1,localhost",
	)
	if err := n.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.stop)
	waitUntil(t, name+" to listen", 40*time.Second, func() bool {
		resp, err := http.Get(n.base() + "/ping")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	n.login()
	return n
}

// stop kills the process by its handle (PID); never by name.
func (n *node) stop() {
	if n.killed {
		return
	}
	n.killed = true
	if n.cmd != nil && n.cmd.Process != nil {
		_ = n.cmd.Process.Kill()
		_, _ = n.cmd.Process.Wait()
	}
	if n.logf != nil {
		_ = n.logf.Close()
	}
}

func (n *node) pid() int { return n.cmd.Process.Pid }

func (n *node) log() string {
	b, _ := os.ReadFile(n.logPath)
	return string(b)
}

func (n *node) count(re string) int {
	return len(regexp.MustCompile(re).FindAllString(n.log(), -1))
}

// ---- HTTP API ----

type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (n *node) do(method, path string, body any) (apiResp, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, n.base()+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if n.token != "" {
		req.Header.Set("Authorization", n.token)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return apiResp{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out apiResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return apiResp{}, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, string(raw))
	}
	return out, nil
}

func (n *node) mustDo(method, path string, body any) apiResp {
	n.t.Helper()
	r, err := n.do(method, path, body)
	if err != nil {
		n.t.Fatalf("%s %s %s: %v", n.name, method, path, err)
	}
	if r.Code != 200 {
		n.t.Fatalf("%s %s %s: code %d: %s", n.name, method, path, r.Code, r.Message)
	}
	return r
}

func (n *node) login() {
	r := n.mustDo("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPass})
	var d struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(r.Data, &d)
	n.token = d.Token
}

// ---- cluster status shapes (only what the tests read) ----

type memberView struct {
	NodeID      string `json:"node_id"`
	Label       string `json:"label"`
	Online      bool   `json:"online"`
	HeldVersion uint64 `json:"held_version"`
	HeldHash    string `json:"held_hash"`
	Laggard     bool   `json:"laggard"`
	Refresher   bool   `json:"refresher"`
	Proto       int    `json:"proto"`
}

type groupView struct {
	ID        string       `json:"id"`
	Version   uint64       `json:"version"`
	CredHash  string       `json:"cred_hash"`
	Members   []memberView `json:"members"`
	Refresher *struct {
		NodeID string `json:"node_id"`
		Valid  bool   `json:"valid"`
		Epoch  uint64 `json:"epoch"`
	} `json:"refresher"`
	LastRefresh *struct {
		NodeID  string `json:"node_id"`
		Trigger string `json:"trigger"`
		Outcome string `json:"outcome"`
		Time    int64  `json:"time"`
	} `json:"last_refresh"`
	Requests []struct {
		Direction string `json:"direction"`
		Result    string `json:"result"`
	} `json:"requests"`
}

type linkView struct {
	NodeID      string `json:"node_id"`
	Connected   bool   `json:"connected"`
	Disconnects int    `json:"disconnects"`
	LastReason  string `json:"last_reason"`
	QueueDepth  int    `json:"queue_depth"`
	RTTMs       int64  `json:"rtt_ms"`
	Proto       int    `json:"proto"`
}

type clusterStatus struct {
	NodeID string      `json:"node_id"`
	Groups []groupView `json:"groups"`
	Links  []linkView  `json:"links"`
}

func (n *node) status() (clusterStatus, error) {
	r, err := n.do("GET", "/api/admin/cluster/status", nil)
	if err != nil {
		return clusterStatus{}, err
	}
	var st clusterStatus
	if err := json.Unmarshal(r.Data, &st); err != nil {
		return st, err
	}
	return st, nil
}

func (n *node) group(id string) (groupView, bool) {
	st, err := n.status()
	if err != nil {
		return groupView{}, false
	}
	for _, g := range st.Groups {
		if g.ID == id {
			return g, true
		}
	}
	return groupView{}, false
}

// list issues a real directory listing of the 115 mount through the HTTP API,
// which drives the 115 Open driver and the SDK against the emulator.
func (n *node) list() error {
	r, err := n.do("POST", "/api/fs/list", map[string]any{"path": "/115", "refresh": true, "page": 1, "per_page": 0})
	if err != nil {
		return err
	}
	if r.Code != 200 {
		return fmt.Errorf("code %d: %s", r.Code, r.Message)
	}
	return nil
}

// ---- cluster assembly ----

type cluster struct {
	t       *testing.T
	root    string
	emu     *emu115.Server
	emuSrv  *httptest.Server
	nodes   []*node
	proxies map[string]*faultProxy
	groupID string
}

func newEmu(t *testing.T, ttl time.Duration) (*emu115.Server, *httptest.Server) {
	emu := emu115.New(emu115.Config{AccessTTL: ttl})
	srv := httptest.NewServer(emu.Handler())
	t.Cleanup(srv.Close)
	return emu, srv
}

// buildCluster starts one hub (nodes[0], accepting inbound) and followers that
// dial it through per-node fault proxies. hubBin selects the hub's binary (to
// run an old build as hub). The hub has a 115 Open storage unless hubStorage is
// false.
func buildCluster(t *testing.T, name string, ttl time.Duration, tm timing, hubBin string, hubStorage bool, followers int) *cluster {
	t.Helper()
	root := filepath.Join(bbRoot(t), name)
	_ = os.RemoveAll(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	emu, emuSrv := newEmu(t, ttl)
	c := &cluster{t: t, root: root, emu: emu, emuSrv: emuSrv, proxies: map[string]*faultProxy{}, groupID: "g115"}
	hub := startNode(t, root, "n1", startOpts{bin: hubBin, emuURL: emuSrv.URL})
	c.nodes = append(c.nodes, hub)
	for i := 0; i < followers; i++ {
		c.nodes = append(c.nodes, startNode(t, root, fmt.Sprintf("n%d", i+2), startOpts{bin: newBin(t), emuURL: emuSrv.URL}))
	}
	key := "bb-cluster-key-" + name
	for i, n := range c.nodes {
		cfg := map[string]any{"enabled": true, "key": key, "label": n.name, "apply_remote": true}
		if i == 0 {
			cfg["announce_interval_sec"] = tm.Announce
		} else {
			p := newFaultProxy(t, fmt.Sprintf("127.0.0.1:%d", c.nodes[0].port))
			c.proxies[n.name] = p
			cfg["seeds"] = []string{"http://" + p.addr()}
			cfg["announce_interval_sec"] = tm.Announce
		}
		if n.bin != oldBin(t) {
			cfg["lease_ttl_sec"] = tm.LeaseTTL
			cfg["refresh_request_wait_sec"] = tm.ReqWait
			cfg["proactive_min_gap_sec"] = tm.MinGap
			cfg["delivery_resend_sec"] = tm.Resend
		}
		n.mustDo("POST", "/api/admin/cluster/config", cfg)
		st, err := n.status()
		if err != nil {
			t.Fatal(err)
		}
		n.nodeID = st.NodeID
	}
	access, refresh := emu.Tokens()
	addition, _ := json.Marshal(map[string]any{"access_token": access, "refresh_token": refresh, "root_folder_id": "0", "limit_rate": 1000, "page_size": 100})
	var members []map[string]string
	for i, n := range c.nodes {
		if i == 0 && !hubStorage {
			continue
		}
		n.mustDo("POST", "/api/admin/storage/create", map[string]any{
			"mount_path": "/115", "driver": "115 Open", "addition": string(addition), "cache_expiration": 0, "order": 0,
		})
		members = append(members, map[string]string{"node_id": n.nodeID, "mount_path": "/115"})
	}
	c.nodes[0].mustDo("POST", "/api/admin/cluster/groups", map[string]any{"groups": []any{map[string]any{"id": c.groupID, "name": "115", "members": members}}})
	waitUntil(t, "group document on every node", 30*time.Second, func() bool {
		for _, n := range c.nodes {
			if _, ok := n.group(c.groupID); !ok {
				return false
			}
		}
		return true
	})
	return c
}

func (c *cluster) byName(name string) *node {
	for _, n := range c.nodes {
		if n.name == name {
			return n
		}
	}
	c.t.Fatalf("no node %s", name)
	return nil
}

func (c *cluster) byID(id string) *node {
	for _, n := range c.nodes {
		if n.nodeID == id {
			return n
		}
	}
	return nil
}

func (c *cluster) setPreferred(preferred *node) {
	c.nodes[0].mustDo("POST", "/api/admin/cluster/refresher", map[string]any{"group_id": c.groupID, "node_id": preferred.nodeID})
}

// holder returns the refresher according to node n's own view.
func (c *cluster) holderSeenBy(n *node) string {
	g, ok := n.group(c.groupID)
	if !ok || g.Refresher == nil || !g.Refresher.Valid {
		return ""
	}
	return g.Refresher.NodeID
}

func (c *cluster) waitHolder(want *node, d time.Duration) time.Duration {
	c.t.Helper()
	start := time.Now()
	waitUntil(c.t, "all live nodes to agree that "+want.name+" is the refresher", d, func() bool {
		for _, n := range c.nodes {
			if n.killed {
				continue
			}
			if c.holderSeenBy(n) != want.nodeID {
				return false
			}
		}
		return true
	})
	return time.Since(start)
}

// refreshOK counts "refresh ok" lines in a node's log: how often *that node*
// spent a refresh_token.
func (n *node) refreshOK() int { return n.count(`\[115_open\] refresh ok`) }

// converged is true when every live member holds the group's current version.
func (c *cluster) converged() bool {
	var want uint64
	for _, n := range c.nodes {
		if n.killed {
			continue
		}
		g, ok := n.group(c.groupID)
		if !ok {
			return false
		}
		if g.Version > want {
			want = g.Version
		}
	}
	if want == 0 {
		return false
	}
	for _, n := range c.nodes {
		if n.killed {
			continue
		}
		g, ok := n.group(c.groupID)
		if !ok || g.Version != want {
			return false
		}
		for _, m := range g.Members {
			if m.Online && m.NodeID != "" && c.byID(m.NodeID) != nil && !c.byID(m.NodeID).killed && m.HeldVersion != want {
				return false
			}
		}
	}
	return true
}

func waitUntil(t *testing.T, what string, d time.Duration, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < d {
		if cond() {
			return time.Since(start)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
	return 0
}

// ---- fault proxy ----

type proxyMode int

const (
	modePass proxyMode = iota
	modeStall
	modeRefuse
)

// faultProxy is a real TCP proxy between a follower and the hub. It can stall
// (hold bytes without forwarding, like a path that silently loses packets),
// kill every connection, or refuse new ones.
type faultProxy struct {
	t      *testing.T
	ln     net.Listener
	target string
	mu     sync.Mutex
	mode   proxyMode
	conns  map[net.Conn]struct{}
	kills  int
}

func newFaultProxy(t *testing.T, target string) *faultProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{t: t, ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	t.Cleanup(func() { ln.Close(); p.killAll() })
	go p.serve()
	return p
}

func (p *faultProxy) addr() string { return p.ln.Addr().String() }

func (p *faultProxy) setMode(m proxyMode) {
	p.mu.Lock()
	p.mode = m
	p.mu.Unlock()
}

func (p *faultProxy) getMode() proxyMode {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode
}

func (p *faultProxy) killAll() {
	p.mu.Lock()
	for c := range p.conns {
		_ = c.Close()
		delete(p.conns, c)
	}
	p.kills++
	p.mu.Unlock()
}

func (p *faultProxy) serve() {
	for {
		in, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.getMode() == modeRefuse {
			_ = in.Close()
			continue
		}
		out, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = in.Close()
			continue
		}
		p.mu.Lock()
		p.conns[in], p.conns[out] = struct{}{}, struct{}{}
		p.mu.Unlock()
		go p.pipe(in, out)
		go p.pipe(out, in)
	}
}

func (p *faultProxy) pipe(src, dst net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			// Stalled: hold the bytes until the path is restored. Nothing is lost,
			// it is just late - like a stalled radio link.
			for p.getMode() == modeStall {
				time.Sleep(50 * time.Millisecond)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	_ = src.Close()
	_ = dst.Close()
	p.mu.Lock()
	delete(p.conns, src)
	delete(p.conns, dst)
	p.mu.Unlock()
}

// ---- result log ----

var resMu sync.Mutex

func record(t *testing.T, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	t.Log(line)
	resMu.Lock()
	defer resMu.Unlock()
	f, err := os.OpenFile(filepath.Join(bbRoot(t), "results.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintf(f, "%s %s: %s\n", time.Now().Format("15:04:05"), t.Name(), line)
		f.Close()
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
