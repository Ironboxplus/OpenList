package frp

import (
	"net"
	"net/http"
	"net/netip"
	"path"
	"strings"
	"time"

	"github.com/pires/go-proxyproto"
)

// The tunnels never point at the public listener. They point at two listeners
// on 127.0.0.1 that serve the same handler:
//
//   - web (http proxies): frps adds the visitor to X-Forwarded-For, after
//     whatever the visitor sent himself, so the LAST entry is the visitor (frp
//     pkg/util/vhost/http.go:60-61). The gateway makes that the request's
//     RemoteAddr and drops all forwarding headers.
//   - raw (tcp/stcp proxies): frpc sends a PROXY protocol v2 header; the
//     listener requires it, so net/http sees the visitor as RemoteAddr. Any
//     forwarding header the visitor sent is dropped.
//
// Both refuse /api/cluster* unless the admin allowed it: the node-to-node
// websocket shares the web listener and must not be reachable from the
// Internet by accident.

type gateway struct {
	addr string
	srv  *http.Server
	ln   net.Listener
}

// forwardingHeaders are what a visitor could use to claim another address.
var forwardingHeaders = []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded", "X-Client-Ip", "True-Client-Ip", "Cf-Connecting-Ip"}

func stripForwarding(h http.Header) {
	for _, k := range forwardingHeaders {
		h.Del(k)
	}
}

func clusterPath(p string) bool {
	c := strings.ToLower(path.Clean("/" + p))
	return c == "/api/cluster" || strings.HasPrefix(c, "/api/cluster/")
}

func filtered(next http.Handler, exposeCluster func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if clusterPath(r.URL.Path) && !exposeCluster() {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// lastForwarded is the last valid address of X-Forwarded-For.
func lastForwarded(h http.Header) (netip.Addr, bool) {
	vals := h.Values("X-Forwarded-For")
	for i := len(vals) - 1; i >= 0; i-- {
		parts := strings.Split(vals[i], ",")
		for j := len(parts) - 1; j >= 0; j-- {
			if a, err := netip.ParseAddr(strings.TrimSpace(parts[j])); err == nil {
				return a.Unmap(), true
			}
		}
	}
	return netip.Addr{}, false
}

func webHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a, ok := lastForwarded(r.Header); ok {
			r.RemoteAddr = net.JoinHostPort(a.String(), "0")
		}
		stripForwarding(r.Header)
		next.ServeHTTP(w, r)
	})
}

func rawHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stripForwarding(r.Header) // RemoteAddr already is the PROXY header's source
		next.ServeHTTP(w, r)
	})
}

func newGateway(ln net.Listener, h http.Handler) *gateway {
	g := &gateway{addr: ln.Addr().String(), ln: ln}
	g.srv = &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = g.srv.Serve(ln) }()
	return g
}

func openWeb(handler http.Handler, exposeCluster func() bool) (*gateway, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return newGateway(ln, webHandler(filtered(handler, exposeCluster))), nil
}

func openRaw(handler http.Handler, exposeCluster func() bool) (*gateway, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	pl := &proxyproto.Listener{
		Listener: ln,
		ConnPolicy: func(proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			return proxyproto.REQUIRE, nil // a connection without the header is refused
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
	return newGateway(pl, rawHandler(filtered(handler, exposeCluster))), nil
}

func (g *gateway) close() {
	if g == nil {
		return
	}
	_ = g.srv.Close()
	_ = g.ln.Close()
}
