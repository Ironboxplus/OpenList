package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A disabled plugin keeps its source on disk under this suffix so Sync skips it
// (only *.go is loaded). Enabling renames it back to <name>.go.
const disabledSuffix = ".go.disabled"

// pluginNameRe constrains plugin names to a safe, path-traversal-proof charset.
var pluginNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidPluginName reports whether name is an acceptable backend plugin name.
func ValidPluginName(name string) bool { return pluginNameRe.MatchString(name) }

// PluginInfo describes a backend Go plugin for the management UI.
type PluginInfo struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Loaded  bool   `json:"loaded"`
	Error   string `json:"error,omitempty"`
}

func (m *Manager) enabledPath(name string) string {
	return filepath.Join(m.goDir(), name+".go")
}

func (m *Manager) disabledPath(name string) string {
	return filepath.Join(m.goDir(), name+disabledSuffix)
}

// ListGoPlugins returns every backend plugin on disk (enabled or disabled) with
// its current load status and last load error, sorted by name.
func (m *Manager) ListGoPlugins() []PluginInfo {
	entries, _ := os.ReadDir(m.goDir())
	seen := map[string]*PluginInfo{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		switch {
		case strings.HasSuffix(n, disabledSuffix):
			name := strings.TrimSuffix(n, disabledSuffix)
			seen[name] = &PluginInfo{Name: name, Enabled: false}
		case strings.HasSuffix(n, ".go"):
			name := strings.TrimSuffix(n, ".go")
			seen[name] = &PluginInfo{Name: name, Enabled: true}
		}
	}
	m.mu.Lock()
	errs := make(map[string]string, len(m.loadErrors))
	for k, v := range m.loadErrors {
		errs[k] = v
	}
	m.mu.Unlock()

	out := make([]PluginInfo, 0, len(seen))
	for name, info := range seen {
		info.Loaded = m.runtime.Loaded(name)
		info.Error = errs[name]
		out = append(out, *info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GetGoPluginSource returns a plugin's source (enabled or disabled variant).
func (m *Manager) GetGoPluginSource(name string) (string, error) {
	if !ValidPluginName(name) {
		return "", fmt.Errorf("invalid plugin name %q", name)
	}
	if b, err := os.ReadFile(m.enabledPath(name)); err == nil {
		return string(b), nil
	}
	b, err := os.ReadFile(m.disabledPath(name))
	if err != nil {
		return "", fmt.Errorf("plugin %q not found", name)
	}
	return string(b), nil
}

// SaveGoPlugin writes (creates or overwrites) a plugin's source and loads it
// immediately. Writing to a currently-disabled plugin keeps it disabled. A
// returned error means the write failed; a successful write whose code fails to
// load still returns nil — inspect the load error via ListGoPlugins.
func (m *Manager) SaveGoPlugin(name, source string) error {
	if !ValidPluginName(name) {
		return fmt.Errorf("invalid plugin name %q (use letters, digits, '-' or '_')", name)
	}
	if err := os.MkdirAll(m.goDir(), 0o755); err != nil {
		return err
	}
	// Preserve enabled/disabled state when overwriting an existing plugin.
	target := m.enabledPath(name)
	if _, err := os.Stat(m.disabledPath(name)); err == nil {
		if _, err := os.Stat(target); err != nil {
			target = m.disabledPath(name)
		}
	}
	if err := os.WriteFile(target, []byte(source), 0o644); err != nil {
		return err
	}
	// Reconcile immediately so the change takes effect without waiting for the
	// next watcher tick.
	_, _ = m.Sync()
	return nil
}

// DeleteGoPlugin removes a plugin (both variants) and unloads it.
func (m *Manager) DeleteGoPlugin(name string) error {
	if !ValidPluginName(name) {
		return fmt.Errorf("invalid plugin name %q", name)
	}
	removed := false
	for _, p := range []string{m.enabledPath(name), m.disabledPath(name)} {
		if err := os.Remove(p); err == nil {
			removed = true
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if !removed {
		return fmt.Errorf("plugin %q not found", name)
	}
	_, _ = m.Sync()
	return nil
}

// SetGoPluginEnabled enables or disables a plugin by renaming its file between
// <name>.go and <name>.go.disabled, then reconciles.
func (m *Manager) SetGoPluginEnabled(name string, enabled bool) error {
	if !ValidPluginName(name) {
		return fmt.Errorf("invalid plugin name %q", name)
	}
	from, to := m.disabledPath(name), m.enabledPath(name)
	if !enabled {
		from, to = m.enabledPath(name), m.disabledPath(name)
	}
	if _, err := os.Stat(from); err != nil {
		// Already in the desired state (or missing) — treat as a no-op success
		// if the target exists, else report not found.
		if _, err2 := os.Stat(to); err2 == nil {
			return nil
		}
		return fmt.Errorf("plugin %q not found", name)
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}
	_, _ = m.Sync()
	return nil
}
