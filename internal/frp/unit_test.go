package frp

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	v1 "github.com/fatedier/frp/pkg/config/v1"
)

func goodConfig() *Config {
	return &Config{
		Enabled: true, ServerAddr: "frps.example.test", ServerPort: 7000, TLS: true, Token: "tok-SECRET-1",
		Proxies: []Proxy{
			{Name: "web", Type: TypeHTTP, CustomDomains: []string{"a.example.test"}},
			{Name: "raw", Type: TypeTCP, RemotePort: 6001},
			{Name: "peer", Type: TypeSTCP, SecretKey: "sk-SECRET-2"},
		},
	}
}

func TestValidate(t *testing.T) {
	if err := goodConfig().Validate(); err != nil {
		t.Fatalf("good config: %v", err)
	}
	bad := map[string]func(*Config){
		"no server":           func(c *Config) { c.ServerAddr = "" },
		"server with path":    func(c *Config) { c.ServerAddr = "a/b" },
		"port 0":              func(c *Config) { c.ServerPort = 0 },
		"port too big":        func(c *Config) { c.ServerPort = 70000 },
		"no proxies":          func(c *Config) { c.Proxies = nil },
		"too many":            func(c *Config) { c.Proxies = make([]Proxy, maxProxies+1) },
		"bad name":            func(c *Config) { c.Proxies[0].Name = "a b" },
		"empty name":          func(c *Config) { c.Proxies[0].Name = "" },
		"duplicate name":      func(c *Config) { c.Proxies[1].Name = "web" },
		"http without domain": func(c *Config) { c.Proxies[0].CustomDomains = nil },
		"tcp without port":    func(c *Config) { c.Proxies[1].RemotePort = 0 },
		"stcp without key":    func(c *Config) { c.Proxies[2].SecretKey = "" },
		"https":               func(c *Config) { c.Proxies[0].Type = "https" },
		"unknown type":        func(c *Config) { c.Proxies[0].Type = "udp" },
	}
	for name, mod := range bad {
		c := goodConfig()
		mod(c)
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: error leaks a secret: %v", name, err)
		}
	}
	// a subdomain is enough for http
	c := goodConfig()
	c.Proxies[0].CustomDomains, c.Proxies[0].Subdomain = nil, "app"
	if err := c.Validate(); err != nil {
		t.Errorf("subdomain: %v", err)
	}
}

func TestBuildTargetsGatewaysWithProxyProtocolForRawOnly(t *testing.T) {
	common, proxies, err := goodConfig().Build("127.0.0.1:11111", "127.0.0.1:22222")
	if err != nil {
		t.Fatal(err)
	}
	if common.ServerAddr != "frps.example.test" || common.ServerPort != 7000 || common.Auth.Token != "tok-SECRET-1" {
		t.Fatalf("common: %+v", common)
	}
	if common.Transport.TLS.Enable == nil || !*common.Transport.TLS.Enable {
		t.Fatal("TLS not enabled")
	}
	if common.LoginFailExit == nil || *common.LoginFailExit {
		t.Fatal("LoginFailExit must be false so a server that is down at boot is retried")
	}
	if len(proxies) != 3 {
		t.Fatalf("%d proxies", len(proxies))
	}
	for _, p := range proxies {
		b := p.GetBaseConfig()
		if b.LocalIP != "127.0.0.1" {
			t.Errorf("%s: local ip %q, not loopback", b.Name, b.LocalIP)
		}
		switch b.Name {
		case "web":
			if b.LocalPort != 11111 || b.Transport.ProxyProtocolVersion != "" {
				t.Errorf("web: %+v", b)
			}
			if h := p.(*v1.HTTPProxyConfig); len(h.CustomDomains) != 1 {
				t.Errorf("web domains: %v", h.CustomDomains)
			}
		case "raw":
			if b.LocalPort != 22222 || b.Transport.ProxyProtocolVersion != "v2" {
				t.Errorf("raw: %+v", b)
			}
			if p.(*v1.TCPProxyConfig).RemotePort != 6001 {
				t.Error("raw remote port")
			}
		case "peer":
			s := p.(*v1.STCPProxyConfig)
			if b.LocalPort != 22222 || b.Transport.ProxyProtocolVersion != "v2" || s.Secretkey != "sk-SECRET-2" {
				t.Errorf("peer: %+v", b)
			}
		}
	}
	// TLS off is honoured
	c := goodConfig()
	c.TLS = false
	common, _, _ = c.Build("127.0.0.1:1", "127.0.0.1:2")
	if common.Transport.TLS.Enable == nil || *common.Transport.TLS.Enable {
		t.Fatal("TLS off not honoured")
	}
}

func TestRingBoundedAndRedacting(t *testing.T) {
	r := &Ring{}
	r.SetSecrets("tok-SECRET-1", "sk-SECRET-2", "")
	for i := range ringLines + 50 {
		fmt.Fprintf(r, "line %d token tok-SECRET-1 and sk-SECRET-2\n", i)
	}
	lines := r.Tail(0)
	if len(lines) != ringLines {
		t.Fatalf("%d lines kept, want %d", len(lines), ringLines)
	}
	if !strings.HasPrefix(lines[0], "line 50 ") || !strings.HasPrefix(lines[len(lines)-1], fmt.Sprintf("line %d ", ringLines+49)) {
		t.Fatalf("not the newest lines: %q ... %q", lines[0], lines[len(lines)-1])
	}
	for _, l := range lines {
		if strings.Contains(l, "SECRET") {
			t.Fatalf("secret stored: %q", l)
		}
	}
	// a line written in pieces is one line; Tail(n) limits
	r2 := &Ring{}
	r2.Write([]byte("hel"))
	r2.Write([]byte("lo\nwor"))
	r2.Write([]byte("ld\n"))
	if got := r2.Tail(10); len(got) != 2 || got[0] != "hello" || got[1] != "world" {
		t.Fatalf("pieces: %q", got)
	}
	if got := r2.Tail(1); len(got) != 1 || got[0] != "world" {
		t.Fatalf("limit: %q", got)
	}
	// our own lines are redacted too
	r.Addf("failed with tok-SECRET-1")
	if l := r.Tail(1)[0]; strings.Contains(l, "SECRET") {
		t.Fatalf("Addf stored a secret: %q", l)
	}
	if got := r.Redact("x tok-SECRET-1 y"); got != "x *** y" {
		t.Fatalf("Redact: %q", got)
	}
}

func TestRingConcurrent(t *testing.T) {
	r := &Ring{}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				fmt.Fprintf(r, "w%d %d\n", i, j)
				_ = r.Tail(5)
				r.SetSecrets("x")
			}
		}()
	}
	wg.Wait()
	if n := len(r.Tail(0)); n != ringLines {
		t.Fatalf("%d lines", n)
	}
}
