package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Config is the operator-facing configuration of the storage-sharing cluster.
// It is persisted as JSON under <data>/cluster/config.json and edited through the
// admin API (and the plugins page UI). Keeping it in its own file rather than the
// global settings table keeps the feature self-contained and pluggable.
type Config struct {
	// Enabled turns the whole cluster sync on/off.
	Enabled bool `json:"enabled"`
	// Key is the cluster pre-shared key. Every node in a cluster must share the
	// exact same value; it is the sole secret that authenticates membership and
	// encrypts traffic. Empty key => sync stays disabled.
	Key string `json:"key"`
	// Peers are the base URLs of the other nodes, e.g. "https://node2.example.com".
	// The cluster endpoints are reached at <peer>/api/cluster/...
	Peers []string `json:"peers"`
	// ShareDrivers, if non-empty, limits sharing to storages of these driver
	// types (e.g. ["115 Cloud","BaiduNetdisk"]). Empty => share every driver.
	ShareDrivers []string `json:"share_drivers"`
	// ShareMounts, if non-empty, limits sharing to these exact mount paths.
	// Empty => no mount-path restriction.
	ShareMounts []string `json:"share_mounts"`
	// ShareDeletes, when true, propagates storage deletions to peers as
	// tombstones. Default false: deleting a mount on one node must NOT silently
	// wipe it cluster-wide — sharing is about credentials/config, not lifecycle.
	ShareDeletes bool `json:"share_deletes"`
	// ApplyRemote, when true, lets incoming peer configs create/update local
	// storages. Default (false) makes a node share-only/observe; set true on
	// nodes that should adopt peer credentials. Most deployments want this true.
	ApplyRemote bool `json:"apply_remote"`
	// AnnounceIntervalSec is how often this node broadcasts its digest so peers
	// can pull anything they missed. 0 => default.
	AnnounceIntervalSec int `json:"announce_interval_sec"`
	// RequestTimeoutSec bounds each outbound peer HTTP request. 0 => default.
	RequestTimeoutSec int `json:"request_timeout_sec"`
}

const (
	defaultAnnounceIntervalSec = 60
	defaultRequestTimeoutSec   = 15
)

func (c *Config) announceInterval() int {
	if c.AnnounceIntervalSec <= 0 {
		return defaultAnnounceIntervalSec
	}
	return c.AnnounceIntervalSec
}

func (c *Config) requestTimeout() int {
	if c.RequestTimeoutSec <= 0 {
		return defaultRequestTimeoutSec
	}
	return c.RequestTimeoutSec
}

// active reports whether sync should actually run: enabled, with a key and at
// least one peer.
func (c *Config) active() bool {
	return c.Enabled && strings.TrimSpace(c.Key) != "" && len(c.peerList()) > 0
}

// peerList returns the trimmed, non-empty peer URLs.
func (c *Config) peerList() []string {
	out := make([]string, 0, len(c.Peers))
	for _, p := range c.Peers {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// shouldShare decides whether a storage of the given driver/mount is in scope.
func (c *Config) shouldShare(driver, mountPath string) bool {
	if len(c.ShareDrivers) > 0 && !contains(c.ShareDrivers, driver) {
		return false
	}
	if len(c.ShareMounts) > 0 && !contains(c.ShareMounts, mountPath) {
		return false
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(strings.TrimSpace(h), needle) {
			return true
		}
	}
	return false
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

// loadOrInit reads config.json, or returns a zero (disabled) config if absent.
func (cs *configStore) loadOrInit() (Config, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	b, err := os.ReadFile(cs.path())
	if err != nil {
		if os.IsNotExist(err) {
			cs.cfg = Config{}
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
