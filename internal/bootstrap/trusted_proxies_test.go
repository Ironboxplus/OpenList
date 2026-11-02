package bootstrap

import (
	"net/http/httptest"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/frp"
	"github.com/gin-gonic/gin"
)

func clientIPOf(t *testing.T, r *gin.Engine, remote string, hdr map[string]string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/ip", nil)
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Body.String()
}

// A client that connects from the Internet cannot choose its own IP with a
// header; a reverse proxy on the host or LAN still can pass the real one on.
func TestMainListenerIgnoresForwardedHeadersFromUntrustedPeers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conf.Conf = conf.DefaultConfig(t.TempDir())
	r := gin.New()
	applyTrustedProxies(r)
	r.GET("/ip", func(c *gin.Context) { c.String(200, c.ClientIP()) })

	spoof := map[string]string{"X-Forwarded-For": "1.2.3.4", "X-Real-IP": "5.6.7.8"}
	for _, remote := range []string{"203.0.113.50:5555", "198.51.100.9:1", "[2001:db8::1]:80"} {
		got := clientIPOf(t, r, remote, spoof)
		if got == "1.2.3.4" || got == "5.6.7.8" {
			t.Errorf("%s: a client-sent header chose the IP: %s", remote, got)
		}
	}
	if got := clientIPOf(t, r, "203.0.113.50:5555", spoof); got != "203.0.113.50" {
		t.Errorf("public peer: %s, want its own address", got)
	}
	// the usual reverse proxy: loopback / private
	for _, remote := range []string{"127.0.0.1:1", "10.1.2.3:1", "192.168.1.5:1", "172.20.0.2:1"} {
		if got := clientIPOf(t, r, remote, map[string]string{"X-Forwarded-For": "198.51.100.77"}); got != "198.51.100.77" {
			t.Errorf("proxy %s: %s, want the forwarded address", remote, got)
		}
	}
	// the proxy is trusted, the visitor behind it is not: his own XFF prefix is skipped
	if got := clientIPOf(t, r, "127.0.0.1:1", map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.77"}); got != "198.51.100.77" {
		t.Errorf("prefix spoof behind a proxy: %s", got)
	}
}

func TestTrustedProxiesAreConfigurableAndBadValuesTrustNobody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conf.Conf = conf.DefaultConfig(t.TempDir())
	conf.Conf.TrustedProxies = []string{"203.0.113.0/24"} // a public load balancer
	r := gin.New()
	applyTrustedProxies(r)
	r.GET("/ip", func(c *gin.Context) { c.String(200, c.ClientIP()) })
	if got := clientIPOf(t, r, "203.0.113.7:1", map[string]string{"X-Forwarded-For": "198.51.100.77"}); got != "198.51.100.77" {
		t.Errorf("configured proxy: %s", got)
	}
	if got := clientIPOf(t, r, "10.0.0.1:1", map[string]string{"X-Forwarded-For": "198.51.100.77"}); got != "10.0.0.1" {
		t.Errorf("a private peer is not trusted once the list is replaced: %s", got)
	}

	conf.Conf.TrustedProxies = []string{"not a cidr"}
	r2 := gin.New()
	applyTrustedProxies(r2)
	r2.GET("/ip", func(c *gin.Context) { c.String(200, c.ClientIP()) })
	if got := clientIPOf(t, r2, "127.0.0.1:1", map[string]string{"X-Forwarded-For": "198.51.100.77"}); got != "127.0.0.1" {
		t.Errorf("bad list should trust nobody: %s", got)
	}
}

func platformEngine(t *testing.T, platform string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	conf.Conf = conf.DefaultConfig(t.TempDir())
	conf.Conf.TrustedPlatform = platform
	r := gin.New()
	applyTrustedProxies(r)
	t.Cleanup(func() { frp.SetPlatformHeader("") })
	r.GET("/ip", func(c *gin.Context) { c.String(200, c.ClientIP()) })
	return r
}

// A CDN in front of the server puts the visitor in its own header; off unless
// asked for.
func TestTrustedPlatform(t *testing.T) {
	cf := map[string]string{"CF-Connecting-IP": "198.51.100.77"}

	// empty: no effect, the header is just a header
	r := platformEngine(t, "")
	if got := clientIPOf(t, r, "104.21.0.9:443", cf); got != "104.21.0.9" {
		t.Errorf("empty setting believed the header: %s", got)
	}
	// cloudflare: the visitor, whoever the peer is
	r = platformEngine(t, "cloudflare")
	if got := clientIPOf(t, r, "104.21.0.9:443", cf); got != "198.51.100.77" {
		t.Errorf("cloudflare: %s", got)
	}
	if got := clientIPOf(t, r, "104.21.0.9:443", nil); got != "104.21.0.9" {
		t.Errorf("cloudflare without the header: %s", got)
	}
	// case-insensitive name, custom header
	r = platformEngine(t, " Cloudflare ")
	if got := clientIPOf(t, r, "104.21.0.9:443", cf); got != "198.51.100.77" {
		t.Errorf("Cloudflare spelled differently: %s", got)
	}
	r = platformEngine(t, "X-Visitor-Ip")
	if got := clientIPOf(t, r, "104.21.0.9:443", map[string]string{"X-Visitor-Ip": "198.51.100.88"}); got != "198.51.100.88" {
		t.Errorf("custom header: %s", got)
	}
	for in, want := range map[string]string{"": "", "cloudflare": "CF-Connecting-IP", "flyio": "Fly-Client-IP", "google": "X-Appengine-Remote-Addr", "X-Foo": "X-Foo"} {
		if got := platformHeader(in); got != want {
			t.Errorf("platformHeader(%q) = %q, want %q", in, got, want)
		}
	}
}
