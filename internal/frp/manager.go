package frp

import (
	"context"
	"net/http"
	"sync"
	"time"

	frpclient "github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config/source"
	frplog "github.com/fatedier/frp/pkg/util/log"
	golog "github.com/fatedier/golib/log"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// Default is the process-wide manager, set by bootstrap.
var Default *Manager

const (
	StateStopped    = "stopped"
	StateConnecting = "connecting"
	StateRunning    = "running"
	StateError      = "error"

	stopWait = 5 * time.Second
)

// ProxyStatus is what frps reports for one proxy.
type ProxyStatus struct {
	Name       string `json:"name"`
	Phase      string `json:"phase"`
	Error      string `json:"error,omitempty"`
	RemoteAddr string `json:"remote_addr,omitempty"`
}

// Status is the manager's state for the admin API.
type Status struct {
	State        string        `json:"state"`
	StateMessage string        `json:"state_message,omitempty"`
	Proxies      []ProxyStatus `json:"proxies"`
	Gateway      string        `json:"gateway,omitempty"`
}

// Manager owns the frp client and its gateways.
type Manager struct {
	handler http.Handler
	ring    *Ring

	mu      sync.Mutex
	gen     int // bumped by every start; a run that ended belongs to an older gen
	cfg     *Config
	svr     *frpclient.Service
	cancel  context.CancelFunc
	done    chan struct{}
	web     *gateway
	raw     *gateway
	state   string
	message string

	// expose is read by the gateways on each request.
	expose func() bool
}

// frp's logger is process-global, so its ring is too.
var sharedRing = &Ring{}

var logOnce sync.Once

// New makes a stopped manager serving handler through its gateways.
func New(handler http.Handler) *Manager {
	m := &Manager{handler: handler, ring: sharedRing, state: StateStopped}
	m.expose = func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.cfg != nil && m.cfg.ExposeCluster
	}
	return m
}

// initLogger points frp's own logger at the ring, once for the process (frp's
// InitLogger would stack a new writer on every start).
func (m *Manager) initLogger() {
	logOnce.Do(func() {
		frplog.Logger = frplog.Logger.WithOptions(golog.WithOutput(m.ring), golog.WithLevel(golog.InfoLevel))
	})
}

// Start loads the configuration from the settings and starts the client.
func (m *Manager) Start() error {
	cfg, err := Load()
	if err != nil {
		m.fail(err)
		return err
	}
	return m.StartWith(cfg)
}

// StartWith starts with cfg (already saved or about to be).
func (m *Manager) StartWith(cfg *Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(cfg)
}

func (m *Manager) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state, m.message = StateError, m.ring.Redact(err.Error())
	m.ring.Addf("start failed: " + m.message)
}

func (m *Manager) startLocked(cfg *Config) error {
	if m.svr != nil {
		return nil // already running
	}
	if err := cfg.Validate(); err != nil {
		m.state, m.message = StateError, m.ring.Redact(err.Error())
		m.ring.Addf("start failed: " + m.message)
		return err
	}
	m.initLogger()
	m.ring.SetSecrets(secretsOf(cfg)...)

	web, err := openWeb(m.handler, m.expose)
	if err != nil {
		return m.failLocked(errors.Wrap(err, "open web gateway"))
	}
	raw, err := openRaw(m.handler, m.expose)
	if err != nil {
		web.close()
		return m.failLocked(errors.Wrap(err, "open raw gateway"))
	}
	common, proxies, err := cfg.Build(web.addr, raw.addr)
	if err != nil {
		web.close()
		raw.close()
		return m.failLocked(err)
	}
	cs := source.NewConfigSource()
	if err := cs.ReplaceAll(proxies, nil); err != nil {
		web.close()
		raw.close()
		return m.failLocked(err)
	}
	svr, err := frpclient.NewService(frpclient.ServiceOptions{
		Common:                 common,
		ConfigSourceAggregator: source.NewAggregator(cs),
	})
	if err != nil {
		web.close()
		raw.close()
		return m.failLocked(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.gen++
	gen := m.gen
	m.cfg, m.svr, m.cancel, m.web, m.raw = cfg, svr, cancel, web, raw
	m.done = make(chan struct{})
	done := m.done
	m.state, m.message = StateConnecting, ""
	m.ring.Addf("starting (" + cfg.ServerAddr + ")")
	go func() {
		defer close(done)
		err := svr.Run(ctx)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.gen != gen {
			return // a newer run owns the state
		}
		if ctx.Err() == nil {
			m.state = StateError
			if err != nil {
				m.message = m.ring.Redact(err.Error())
			} else {
				m.message = "client stopped"
			}
			m.ring.Addf("client stopped unexpectedly: " + m.message)
			log.Warnf("frp client stopped unexpectedly: %s", m.message)
			m.releaseLocked()
		}
	}()
	return nil
}

func (m *Manager) failLocked(err error) error {
	m.state, m.message = StateError, m.ring.Redact(err.Error())
	m.ring.Addf("start failed: " + m.message)
	return err
}

// releaseLocked drops the service and closes the gateways (state is left to
// the caller).
func (m *Manager) releaseLocked() {
	m.svr, m.cancel = nil, nil
	m.web.close()
	m.raw.close()
	m.web, m.raw = nil, nil
}

// Stop stops the client and waits (a few seconds at most) for it to finish.
func (m *Manager) Stop() {
	m.mu.Lock()
	svr, cancel, done := m.svr, m.cancel, m.done
	if svr == nil {
		if m.state != StateStopped {
			m.state, m.message = StateStopped, ""
		}
		m.mu.Unlock()
		return
	}
	m.gen++ // whatever Run reports from now on is not an error
	m.releaseLocked()
	m.state, m.message = StateStopped, ""
	m.mu.Unlock()

	cancel()
	svr.Close()
	select {
	case <-done:
	case <-time.After(stopWait):
		m.ring.Addf("stop: client did not finish within 5s, continuing")
	}
	m.ring.Addf("stopped")
}

// Apply makes the running client match cfg: only the proxy list changed ->
// reloaded in place; anything else -> restart; disabled -> stop.
func (m *Manager) Apply(cfg *Config) error {
	if !cfg.Enabled {
		m.Stop()
		return nil
	}
	m.mu.Lock()
	cur := m.cfg
	running := m.svr != nil
	m.mu.Unlock()
	if !running || cur == nil || !cur.sameConnection(cfg) {
		m.Stop()
		return m.StartWith(cfg)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.svr == nil {
		return m.startLocked(cfg)
	}
	common, proxies, err := cfg.Build(m.web.addr, m.raw.addr)
	if err != nil {
		return err
	}
	if err := m.svr.UpdateConfigSource(common, proxies, nil); err != nil {
		return errors.Wrap(err, "reload proxies")
	}
	m.cfg = cfg
	m.ring.SetSecrets(secretsOf(cfg)...)
	m.ring.Addf("proxy list reloaded")
	return nil
}

func secretsOf(c *Config) []string {
	out := []string{c.Token}
	for _, p := range c.Proxies {
		out = append(out, p.SecretKey)
	}
	return out
}

// Status reports the state; proxy phases are asked from the client now.
func (m *Manager) Status() Status {
	m.mu.Lock()
	svr, cfg := m.svr, m.cfg
	st := Status{State: m.state, StateMessage: m.message, Proxies: []ProxyStatus{}}
	if m.web != nil {
		st.Gateway = m.web.addr
	}
	m.mu.Unlock()
	if svr == nil || cfg == nil {
		return st
	}
	exp := svr.StatusExporter()
	running := false
	for _, name := range cfg.proxyNames() {
		ps := ProxyStatus{Name: name, Phase: "unknown"}
		if w, ok := exp.GetProxyStatus(name); ok {
			ps.Phase, ps.Error, ps.RemoteAddr = w.Phase, m.ring.Redact(w.Err), w.RemoteAddr
			if w.Phase == "running" {
				running = true
			}
		}
		st.Proxies = append(st.Proxies, ps)
	}
	if st.State == StateConnecting || st.State == StateRunning {
		if running {
			st.State = StateRunning
		} else {
			st.State = StateConnecting
		}
	}
	return st
}

// Runtime is the recent log.
func (m *Manager) Runtime(limit int) []string { return m.ring.Tail(limit) }

// Ring exposes the log ring (tests).
func (m *Manager) Ring() *Ring { return m.ring }

// GatewayAddrs returns the web and raw gateway addresses ("" when stopped).
func (m *Manager) GatewayAddrs() (web, raw string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.web != nil {
		web = m.web.addr
	}
	if m.raw != nil {
		raw = m.raw.addr
	}
	return
}
