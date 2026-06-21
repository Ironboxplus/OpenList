package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Config is the operator-facing configuration of the credential-sharing cluster.
// It is persisted as JSON under <data>/cluster/config.json and edited through the
// admin API (and the plugins page UI). Keeping it in its own file rather than the
// global settings table keeps the feature self-contained and pluggable.
//
// The redesign narrows sharing to CREDENTIALS only (tokens/cookies/secrets) and
// routes them through explicit, manually-defined sync groups (see groups.go).
// Peer membership is auto-discovered: any node that can open our AEAD envelope is
// admitted, public nodes advertise their address, and the rest is learned via
// peer exchange. The only manual knobs left are the shared key, an optional
// self-advertised address, and optional bootstrap seeds for the first join.
type Config struct {
	// Enabled turns the whole cluster sync on/off.
	Enabled bool `json:"enabled"`
	// Key is the cluster pre-shared key. Every node in a cluster must share the
	// exact same value; it is the sole secret that authenticates membership and
	// encrypts traffic. Empty key => sync stays disabled.
	Key string `json:"key"`
	// Label is a human-friendly name for THIS node shown in the cluster UI of all
	// nodes (e.g. "cfscan", "home-nas"). Falls back to the node id when empty.
	Label string `json:"label"`
	// Addr is this node's externally reachable base URL (e.g.
	// "https://node1.example.com"). Advertising it lets other nodes auto-discover
	// and dial this node. Leave empty for nodes behind NAT — they dial out and are
	// reached through a connected public node's relay instead.
	Addr string `json:"addr"`
	// Seeds are optional bootstrap base URLs to dial when joining an existing
	// cluster. Only needed once: after the first connection, every reachable
	// node's address is learned automatically via peer exchange.
	Seeds []string `json:"seeds"`
	// ApplyRemote, when true (the default for a credential cluster), lets incoming
	// peer credentials update local storages in a shared group. Set false to make
	// a node observe/share-only.
	ApplyRemote bool `json:"apply_remote"`
	// AnnounceIntervalSec is how often this node re-broadcasts its inventory +
	// credential digests so peers converge after any lost message. 0 => default.
	AnnounceIntervalSec int `json:"announce_interval_sec"`
}

const (
	defaultAnnounceIntervalSec = 45
)

func (c Config) announceInterval() int {
	if c.AnnounceIntervalSec <= 0 {
		return defaultAnnounceIntervalSec
	}
	return c.AnnounceIntervalSec
}

// active reports whether sync should actually run: enabled with a key. Unlike the
// previous design it does NOT require a configured peer — a reachable node can run
// purely accept-only and still serve a whole cluster.
func (c Config) active() bool {
	return c.Enabled && strings.TrimSpace(c.Key) != ""
}

// seedList returns the trimmed, non-empty bootstrap URLs.
func (c Config) seedList() []string {
	return cleanURLs(c.Seeds)
}

func cleanURLs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, p := range in {
		p = strings.TrimRight(strings.TrimSpace(p), "/")
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// configStore handles persistence of Config under a directory.
type configStore struct {
	mu  sync.RWMutex
	dir string
	cfg Config
}

func newConfigStore(dir string) *configStore {
	return &configStore{dir: dir}
}

func (cs *configStore) path() string { return filepath.Join(cs.dir, "config.json") }

// loadOrInit reads config.json, or returns a default (disabled, apply-remote-on)
// config if absent.
func (cs *configStore) loadOrInit() (Config, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	b, err := os.ReadFile(cs.path())
	if err != nil {
		if os.IsNotExist(err) {
			cs.cfg = Config{ApplyRemote: true}
			return cs.cfg, nil
		}
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	cs.cfg = c
	return c, nil
}

func (cs *configStore) get() Config {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cfg
}

// save writes config.json atomically (write temp + rename).
func (cs *configStore) save(c Config) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if err := os.MkdirAll(cs.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := cs.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, cs.path()); err != nil {
		return err
	}
	cs.cfg = c
	return nil
}
