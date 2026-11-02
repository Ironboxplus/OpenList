package bootstrap

import (
	"net/http/httptest"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
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
