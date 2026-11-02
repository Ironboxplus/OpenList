// Package frp runs an frp client inside the process (see PLAN_GLUE_ALIST.md §3,
// tmp/glue-frp-design.md). It is off unless the admin turns it on; when on, the
// tunnels never point at the public listener but at two loopback "gateway"
// listeners (gateway.go) that recover the real visitor IP and keep the cluster
// endpoint closed.
package frp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/pkg/errors"
)

const (
	TypeHTTP = "http"
	TypeTCP  = "tcp"
	TypeSTCP = "stcp"

	maxProxies = 16
)

var proxyNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Proxy is one tunnel. SecretKey (stcp) is kept apart from the rest in storage
// and never leaves the process (see View).
type Proxy struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	CustomDomains []string `json:"custom_domains,omitempty"`
	Subdomain     string   `json:"subdomain,omitempty"`
	RemotePort    int      `json:"remote_port,omitempty"`
	SecretKey     string   `json:"-"`
}

// Config is the whole frp configuration as stored in the settings.
type Config struct {
	Enabled       bool
	ServerAddr    string
	ServerPort    int
	TLS           bool
	Token         string
	ExposeCluster bool
	Proxies       []Proxy
}

// Load reads the configuration from the settings.
func Load() (*Config, error) {
	c := &Config{
		Enabled:       setting.GetBool(conf.FRPEnabled),
		ServerAddr:    strings.TrimSpace(setting.GetStr(conf.FRPServerAddr)),
		ServerPort:    setting.GetInt(conf.FRPServerPort, 7000),
		TLS:           setting.GetBool(conf.FRPTLSEnable),
		Token:         setting.GetStr(conf.FRPAuthToken),
		ExposeCluster: setting.GetBool(conf.FRPExposeCluster),
	}
	if raw := strings.TrimSpace(setting.GetStr(conf.FRPProxies)); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.Proxies); err != nil {
			return nil, errors.Errorf("frp_proxies is not valid JSON: %v", err)
		}
	}
	secrets := map[string]string{}
	if raw := strings.TrimSpace(setting.GetStr(conf.FRPProxySecrets)); raw != "" {
		if err := json.Unmarshal([]byte(raw), &secrets); err != nil {
			return nil, errors.Errorf("frp_proxy_secrets is not valid JSON")
		}
	}
	for i := range c.Proxies {
		c.Proxies[i].SecretKey = secrets[c.Proxies[i].Name]
	}
	return c, nil
}

// Validate checks everything that can be checked without a server. Messages
// never contain the token or a secret key.
func (c *Config) Validate() error {
	if c.ServerAddr == "" {
		return errors.New("server address is required")
	}
	if strings.ContainsAny(c.ServerAddr, " /\t\r\n") {
		return errors.New("server address must be a host name or IP")
	}
	if c.ServerPort < 1 || c.ServerPort > 65535 {
		return errors.New("server port must be 1..65535")
	}
	if len(c.Proxies) == 0 {
		return errors.New("add at least one proxy")
	}
	if len(c.Proxies) > maxProxies {
		return errors.Errorf("at most %d proxies", maxProxies)
	}
	seen := map[string]bool{}
	for _, p := range c.Proxies {
		if !proxyNameRe.MatchString(p.Name) {
			return errors.Errorf("proxy name %q must be 1..64 of letters, digits, _ and -", p.Name)
		}
		if seen[p.Name] {
			return errors.Errorf("proxy name %q is used twice", p.Name)
		}
		seen[p.Name] = true
		switch p.Type {
		case TypeHTTP:
			if len(p.CustomDomains) == 0 && p.Subdomain == "" {
				return errors.Errorf("proxy %q: an http proxy needs a custom domain or a subdomain", p.Name)
			}
		case TypeTCP:
			if p.RemotePort < 1 || p.RemotePort > 65535 {
				return errors.Errorf("proxy %q: a tcp proxy needs a remote port (1..65535)", p.Name)
			}
		case TypeSTCP:
			if p.SecretKey == "" {
				return errors.Errorf("proxy %q: an stcp proxy needs a secret key", p.Name)
			}
		case "https":
			return errors.Errorf("proxy %q: https proxies are not supported (they would need a certificate here); use http behind your frps TLS or a tcp proxy", p.Name)
		default:
			return errors.Errorf("proxy %q: unsupported type %q (http, tcp, stcp)", p.Name, p.Type)
		}
	}
	return nil
}

// Build turns the configuration into frp's own. web and raw are the addresses
// (host:port) of the two gateways: http proxies go to web, tcp/stcp to raw, with
// PROXY protocol v2 so the real visitor address reaches us.
func (c *Config) Build(web, raw string) (*v1.ClientCommonConfig, []v1.ProxyConfigurer, error) {
	webHost, webPort, err := splitHostPort(web)
	if err != nil {
		return nil, nil, err
	}
	rawHost, rawPort, err := splitHostPort(raw)
	if err != nil {
		return nil, nil, err
	}
	loginRetry := false
	common := &v1.ClientCommonConfig{
		ServerAddr:    c.ServerAddr,
		ServerPort:    c.ServerPort,
		Auth:          v1.AuthClientConfig{Method: v1.AuthMethodToken, Token: c.Token},
		LoginFailExit: &loginRetry, // keep trying: the network may come up after us
	}
	tls := c.TLS
	common.Transport.TLS.Enable = &tls

	out := make([]v1.ProxyConfigurer, 0, len(c.Proxies))
	for _, p := range c.Proxies {
		switch p.Type {
		case TypeHTTP:
			cfg := &v1.HTTPProxyConfig{}
			cfg.Name, cfg.Type = p.Name, TypeHTTP
			cfg.LocalIP, cfg.LocalPort = webHost, webPort
			cfg.CustomDomains = append([]string(nil), p.CustomDomains...)
			cfg.SubDomain = p.Subdomain
			out = append(out, cfg)
		case TypeTCP:
			cfg := &v1.TCPProxyConfig{}
			cfg.Name, cfg.Type = p.Name, TypeTCP
			cfg.LocalIP, cfg.LocalPort = rawHost, rawPort
			cfg.Transport.ProxyProtocolVersion = "v2"
			cfg.RemotePort = p.RemotePort
			out = append(out, cfg)
		case TypeSTCP:
			cfg := &v1.STCPProxyConfig{}
			cfg.Name, cfg.Type = p.Name, TypeSTCP
			cfg.LocalIP, cfg.LocalPort = rawHost, rawPort
			cfg.Transport.ProxyProtocolVersion = "v2"
			cfg.Secretkey = p.SecretKey
			cfg.AllowUsers = []string{"*"}
			out = append(out, cfg)
		default:
			return nil, nil, errors.Errorf("unsupported proxy type %q", p.Type)
		}
	}
	return common, out, nil
}

func splitHostPort(addr string) (string, int, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("bad gateway address %q", addr)
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("bad gateway address %q", addr)
	}
	return addr[:i], port, nil
}

// sameConnection reports whether two configurations differ only in the proxy
// list, which can be reloaded without dropping the control connection.
func (c *Config) sameConnection(o *Config) bool {
	return c.ServerAddr == o.ServerAddr && c.ServerPort == o.ServerPort &&
		c.TLS == o.TLS && c.Token == o.Token
}

// proxyNames is the sorted list of proxy names (for status output).
func (c *Config) proxyNames() []string {
	names := make([]string, 0, len(c.Proxies))
	for _, p := range c.Proxies {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}
