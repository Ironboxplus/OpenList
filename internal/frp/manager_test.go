package frp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	frpserver "github.com/fatedier/frp/server"
)

// These tests run a REAL frps in the process (frp ships its server as a
// library) on loopback and drive the manager against it. No mocks, no network
// outside 127.0.0.0/8.

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type testFrps struct {
	bind, vhost int
	cancel      context.CancelFunc
	svr         *frpserver.Service
	done        chan struct{}
}

func startFrps(t *testing.T, token string, bind, vhost int) *testFrps {
	t.Helper()
	cfg := &v1.ServerConfig{BindAddr: "127.0.0.1", ProxyBindAddr: "127.0.0.1", BindPort: bind, VhostHTTPPort: vhost}
	cfg.Auth.Token = token
	if err := cfg.Complete(); err != nil {
		t.Fatal(err)
	}
	cfg.BindPort = bind
	svr, err := frpserver.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &testFrps{bind: bind, vhost: vhost, cancel: cancel, svr: svr, done: make(chan struct{})}
	go func() { defer close(f.done); svr.Run(ctx) }()
	t.Cleanup(f.stop)
	return f
}

func (f *testFrps) stop() {
	f.cancel()
	_ = f.svr.Close()
}

func waitState(t *testing.T, m *Manager, want string, within time.Duration) Status {
	t.Helper()
	deadline := time.Now().Add(within)
	var st Status
	for time.Now().Before(deadline) {
		st = m.Status()
		if st.State == want {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("state %q (%s), want %q; log: %v", st.State, st.StateMessage, want, m.Runtime(8))
	return st
}

func newTestManager(t *testing.T) *Manager {
	m := New(testApp())
	t.Cleanup(m.Stop)
	return m
}

func cfgFor(f *testFrps, token string, proxies ...Proxy) *Config {
	return &Config{Enabled: true, ServerAddr: "127.0.0.1", ServerPort: f.bind, TLS: true, Token: token, Proxies: proxies}
}

// fromIP dials dst with source address src (127.0.0.0/8 is all loopback), so
// the visitor is not 127.0.0.1 like the gateway itself.
func fromIP(src, dst string) (net.Conn, error) {
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}, Timeout: 3 * time.Second}
	return d.Dial("tcp", dst)
}

func httpVia(t *testing.T, src, addr, host, path string, hdr map[string]string) (int, string) {
	t.Helper()
	tr := &http.Transport{DialContext: func(ctx context.Context, n, a string) (net.Conn, error) { return fromIP(src, a) }, DisableKeepAlives: true}
	req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitProxiesRunning(t *testing.T, m *Manager, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ok := 0
		for _, p := range m.Status().Proxies {
			if p.Phase == "running" {
				ok++
			}
		}
		if ok >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxies: %+v", m.Status().Proxies)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTunnelsDeliverTheRealVisitorAddress(t *testing.T) {
	f := startFrps(t, "tok-SECRET-1", freePort(t), freePort(t))
	rawPort := freePort(t)
	m := newTestManager(t)
	if err := m.StartWith(cfgFor(f, "tok-SECRET-1",
		Proxy{Name: "web", Type: TypeHTTP, CustomDomains: []string{"app.example.test"}},
		Proxy{Name: "raw", Type: TypeTCP, RemotePort: rawPort},
	)); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, m, StateRunning, 15*time.Second)
	if !strings.HasPrefix(st.Gateway, "127.0.0.1:") {
		t.Fatalf("gateway: %q", st.Gateway)
	}
	waitProxiesRunning(t, m, 2)

	// http proxy: the visitor is 127.0.0.7; whatever XFF he sends, frps appends him last
	code, body := httpVia(t, "127.0.0.7", fmt.Sprintf("127.0.0.1:%d", f.vhost), "app.example.test", "/ip",
		map[string]string{"X-Forwarded-For": "6.6.6.6", "X-Real-IP": "7.7.7.7"})
	if code != 200 || !strings.HasPrefix(body, "127.0.0.7|") {
		t.Fatalf("http path: %d %s", code, body)
	}
	if !strings.Contains(body, `xff=""`) {
		t.Fatalf("forwarding header reached the app: %s", body)
	}

	// tcp proxy: PROXY protocol v2 carries the visitor; his own XFF is ignored
	c, err := fromIP("127.0.0.9", fmt.Sprintf("127.0.0.1:%d", rawPort))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	code, body = rawGet(t, c, "/ip", "X-Forwarded-For: 6.6.6.6\r\n")
	if code != 200 || !strings.HasPrefix(body, "127.0.0.9|") {
		t.Fatalf("tcp path: %d %s", code, body)
	}

	// the cluster endpoint is not reachable through either tunnel
	if code, _ := httpVia(t, "127.0.0.7", fmt.Sprintf("127.0.0.1:%d", f.vhost), "app.example.test", "/api/cluster/ws", nil); code != 404 {
		t.Fatalf("cluster through http tunnel: %d", code)
	}
	c2, err := fromIP("127.0.0.9", fmt.Sprintf("127.0.0.1:%d", rawPort))
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if code, _ := rawGet(t, c2, "/api/cluster/ws", ""); code != 404 {
		t.Fatalf("cluster through tcp tunnel: %d", code)
	}
}

func TestExposeClusterIsOptIn(t *testing.T) {
	f := startFrps(t, "tok", freePort(t), freePort(t))
	m := newTestManager(t)
	cfg := cfgFor(f, "tok", Proxy{Name: "web", Type: TypeHTTP, CustomDomains: []string{"app.example.test"}})
	cfg.ExposeCluster = true
	if err := m.StartWith(cfg); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, StateRunning, 15*time.Second)
	waitProxiesRunning(t, m, 1)
	if code, body := httpVia(t, "127.0.0.7", fmt.Sprintf("127.0.0.1:%d", f.vhost), "app.example.test", "/api/cluster/ws", nil); code != 200 || body != "cluster" {
		t.Fatalf("exposed cluster: %d %s", code, body)
	}
}

func TestWrongTokenNeverRuns(t *testing.T) {
	f := startFrps(t, "right-token", freePort(t), freePort(t))
	m := newTestManager(t)
	if err := m.StartWith(cfgFor(f, "wrong-SECRET-token", Proxy{Name: "web", Type: TypeHTTP, CustomDomains: []string{"a.example.test"}})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	st := m.Status()
	if st.State == StateRunning {
		t.Fatalf("running with a wrong token")
	}
	all := st.StateMessage + strings.Join(m.Runtime(0), "\n")
	if strings.Contains(all, "wrong-SECRET-token") || strings.Contains(all, "right-token") {
		t.Fatalf("a token reached the message or the log: %s", all)
	}
}

func TestServerDownAtBootIsRetried(t *testing.T) {
	bind, vhost := freePort(t), freePort(t)
	m := newTestManager(t)
	f := &testFrps{bind: bind}
	if err := m.StartWith(cfgFor(f, "tok", Proxy{Name: "web", Type: TypeHTTP, CustomDomains: []string{"a.example.test"}})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if st := m.Status(); st.State != StateConnecting {
		t.Fatalf("with the server down: %s (%s), want %s", st.State, st.StateMessage, StateConnecting)
	}
	startFrps(t, "tok", bind, vhost) // the server comes up later
	waitState(t, m, StateRunning, 30*time.Second)
}

func TestProxyListReloadKeepsTheConnection(t *testing.T) {
	f := startFrps(t, "tok", freePort(t), freePort(t))
	m := newTestManager(t)
	cfg := cfgFor(f, "tok", Proxy{Name: "one", Type: TypeHTTP, CustomDomains: []string{"one.example.test"}})
	if err := m.StartWith(cfg); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, StateRunning, 15*time.Second)
	m.mu.Lock()
	svrBefore := m.svr
	m.mu.Unlock()

	next := cfgFor(f, "tok",
		Proxy{Name: "one", Type: TypeHTTP, CustomDomains: []string{"one.example.test"}},
		Proxy{Name: "two", Type: TypeHTTP, CustomDomains: []string{"two.example.test"}})
	if err := m.Apply(next); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	same := m.svr == svrBefore
	m.mu.Unlock()
	if !same {
		t.Fatal("a proxy-list change restarted the client")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body := httpVia(t, "127.0.0.7", fmt.Sprintf("127.0.0.1:%d", f.vhost), "two.example.test", "/ip", nil)
		if code == 200 && strings.HasPrefix(body, "127.0.0.7|") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new proxy not served: %d %s", code, body)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// a different server address needs a new client
	other := startFrps(t, "tok", freePort(t), freePort(t))
	moved := cfgFor(other, "tok", next.Proxies...)
	if err := m.Apply(moved); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	restarted := m.svr != svrBefore
	m.mu.Unlock()
	if !restarted {
		t.Fatal("a new server address did not restart the client")
	}
	waitState(t, m, StateRunning, 15*time.Second)

	// disabled -> stopped, gateways closed
	moved.Enabled = false
	if err := m.Apply(moved); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.State != StateStopped {
		t.Fatalf("after disable: %s", st.State)
	}
	if w, r := m.GatewayAddrs(); w != "" || r != "" {
		t.Fatalf("gateways still open: %s %s", w, r)
	}
}

func TestStartStopCyclesDoNotLeak(t *testing.T) {
	f := startFrps(t, "tok", freePort(t), freePort(t))
	m := newTestManager(t)
	cfg := cfgFor(f, "tok", Proxy{Name: "web", Type: TypeHTTP, CustomDomains: []string{"a.example.test"}})
	cycle := func() {
		if err := m.StartWith(cfg); err != nil {
			t.Fatal(err)
		}
		waitState(t, m, StateRunning, 15*time.Second)
		m.Stop()
		if st := m.Status(); st.State != StateStopped {
			t.Fatalf("after stop: %s", st.State)
		}
	}
	cycle() // warm up pools and the logger
	runtime.GC()
	time.Sleep(500 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 15 {
		cycle()
	}
	time.Sleep(time.Second)
	runtime.GC()
	if after := runtime.NumGoroutine(); after > before+10 {
		t.Fatalf("goroutines grew from %d to %d over 15 start/stop cycles", before, after)
	}
	// and it still starts after all that, with nothing stale flipping the state
	if err := m.StartWith(cfg); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, StateRunning, 15*time.Second)
}

func TestStopReturnsWhenTheServerIsUnreachable(t *testing.T) {
	m := newTestManager(t)
	cfg := &Config{Enabled: true, ServerAddr: "127.0.0.1", ServerPort: freePort(t), TLS: true, Token: "t",
		Proxies: []Proxy{{Name: "web", Type: TypeHTTP, CustomDomains: []string{"a.example.test"}}}}
	if err := m.StartWith(cfg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	m.Stop()
	if d := time.Since(start); d > stopWait+time.Second {
		t.Fatalf("Stop took %s", d)
	}
	if st := m.Status(); st.State != StateStopped {
		t.Fatalf("state %s", st.State)
	}
}

func TestInvalidConfigDoesNotStartAnything(t *testing.T) {
	m := newTestManager(t)
	err := m.StartWith(&Config{Enabled: true, ServerAddr: "", ServerPort: 7000})
	if err == nil {
		t.Fatal("started with no server")
	}
	if st := m.Status(); st.State != StateError || st.StateMessage == "" {
		t.Fatalf("state %+v", st)
	}
	if w, r := m.GatewayAddrs(); w != "" || r != "" {
		t.Fatalf("gateways open for a config that did not start")
	}
}

func TestDefaultOffOpensNothing(t *testing.T) {
	before := runtime.NumGoroutine()
	m := New(testApp())
	if st := m.Status(); st.State != StateStopped {
		t.Fatalf("new manager: %s", st.State)
	}
	if w, r := m.GatewayAddrs(); w != "" || r != "" {
		t.Fatal("gateways open on a manager that was never started")
	}
	if runtime.NumGoroutine() > before {
		t.Fatalf("New started goroutines")
	}
}
