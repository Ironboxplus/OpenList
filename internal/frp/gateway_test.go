package frp

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pires/go-proxyproto"
)

// testApp is a stand-in for the web handler: the same trusted-proxy rule as the
// real server, one route that reports the client IP gin computes, the cluster
// websocket path, and a header echo.
func testApp() http.Handler {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	_ = r.SetTrustedProxies([]string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7", "fe80::/10"})
	r.GET("/ip", func(c *gin.Context) {
		c.String(200, "%s|xff=%q|real=%q", c.ClientIP(), c.GetHeader("X-Forwarded-For"), c.GetHeader("X-Real-IP"))
	})
	r.GET("/api/cluster/ws", func(c *gin.Context) { c.String(200, "cluster") })
	r.GET("/api/cluster", func(c *gin.Context) { c.String(200, "cluster") })
	return r
}

func get(t *testing.T, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestWebGatewayTakesTheLastForwardedAddress(t *testing.T) {
	g, err := openWeb(testApp(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	if !strings.HasPrefix(g.addr, "127.0.0.1:") {
		t.Fatalf("gateway is not on loopback: %s", g.addr)
	}
	// frps appends the visitor after what the visitor sent
	code, body := get(t, "http://"+g.addr+"/ip", map[string]string{
		"X-Forwarded-For": "6.6.6.6, 10.1.1.1, 203.0.113.9", "X-Real-IP": "7.7.7.7", "Forwarded": "for=8.8.8.8",
	})
	if code != 200 || !strings.HasPrefix(body, "203.0.113.9|") {
		t.Fatalf("client ip: %d %s", code, body)
	}
	if !strings.Contains(body, `xff=""`) || !strings.Contains(body, `real=""`) {
		t.Fatalf("forwarding headers reached the app: %s", body)
	}
	// a request without any: the connection's own address
	_, body = get(t, "http://"+g.addr+"/ip", nil)
	if !strings.HasPrefix(body, "127.0.0.1|") {
		t.Fatalf("no header: %s", body)
	}
	// garbage in the header is ignored, not trusted
	_, body = get(t, "http://"+g.addr+"/ip", map[string]string{"X-Forwarded-For": "not-an-ip"})
	if !strings.HasPrefix(body, "127.0.0.1|") {
		t.Fatalf("garbage: %s", body)
	}
	// IPv6, and an IPv4-mapped one is unmapped
	_, body = get(t, "http://"+g.addr+"/ip", map[string]string{"X-Forwarded-For": "2001:db8::5"})
	if !strings.HasPrefix(body, "2001:db8::5|") {
		t.Fatalf("ipv6: %s", body)
	}
	_, body = get(t, "http://"+g.addr+"/ip", map[string]string{"X-Forwarded-For": "::ffff:198.51.100.7"})
	if !strings.HasPrefix(body, "198.51.100.7|") {
		t.Fatalf("mapped: %s", body)
	}
}

func rawConn(t *testing.T, addr string, src string, header bool) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if header {
		sa := net.ParseIP(src)
		h := proxyproto.HeaderProxyFromAddrs(2,
			&net.TCPAddr{IP: sa, Port: 4242}, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80})
		if _, err := h.WriteTo(c); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func rawGet(t *testing.T, c net.Conn, path string, hdr string) (int, string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: x\r\nConnection: close\r\n"+hdr+"\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRawGatewayReadsTheProxyProtocolHeader(t *testing.T) {
	g, err := openRaw(testApp(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	if !strings.HasPrefix(g.addr, "127.0.0.1:") {
		t.Fatalf("gateway is not on loopback: %s", g.addr)
	}
	c := rawConn(t, g.addr, "203.0.113.77", true)
	defer c.Close()
	// the visitor also sends forwarding headers of his own: ignored
	code, body := rawGet(t, c, "/ip", "X-Forwarded-For: 6.6.6.6\r\nX-Real-IP: 7.7.7.7\r\n")
	if code != 200 || !strings.HasPrefix(body, "203.0.113.77|") || !strings.Contains(body, `xff=""`) || !strings.Contains(body, `real=""`) {
		t.Fatalf("raw gateway: %d %s", code, body)
	}
}

func TestRawGatewayRefusesAConnectionWithoutTheHeader(t *testing.T) {
	g, err := openRaw(testApp(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	c := rawConn(t, g.addr, "", false)
	defer c.Close()
	if code, body := rawGet(t, c, "/ip", ""); code != 0 {
		t.Fatalf("served without a PROXY header: %d %s", code, body)
	}
}

func TestGatewaysKeepTheClusterEndpointClosed(t *testing.T) {
	expose := false
	web, err := openWeb(testApp(), func() bool { return expose })
	if err != nil {
		t.Fatal(err)
	}
	defer web.close()
	raw, err := openRaw(testApp(), func() bool { return expose })
	if err != nil {
		t.Fatal(err)
	}
	defer raw.close()

	paths := []string{"/api/cluster/ws", "/api/cluster", "/API/Cluster/ws", "//api/cluster/ws", "/api/x/../cluster/ws", "/api/cluster/"}
	for _, p := range paths {
		if code, _ := get(t, "http://"+web.addr+p, nil); code != 404 {
			t.Errorf("web %s: %d, want 404", p, code)
		}
		c := rawConn(t, raw.addr, "198.51.100.1", true)
		if code, _ := rawGet(t, c, p, ""); code != 404 {
			t.Errorf("raw %s: %d, want 404", p, code)
		}
		c.Close()
	}
	if code, _ := get(t, "http://"+web.addr+"/ip", nil); code != 200 {
		t.Errorf("an ordinary path is blocked: %d", code)
	}
	expose = true
	if code, body := get(t, "http://"+web.addr+"/api/cluster/ws", nil); code != 200 || body != "cluster" {
		t.Errorf("exposed: %d %s", code, body)
	}
}
