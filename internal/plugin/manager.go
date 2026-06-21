package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// ManifestEntry describes a frontend (JS) plugin the backend serves to the web UI.
type ManifestEntry struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// Manifest is the payload the frontend fetches to hot-load JS plugins.
type Manifest struct {
	Plugins []ManifestEntry `json:"plugins"`
}

type fileState struct {
	mod  time.Time
	size int64
}

// Manager watches a plugins directory and keeps the yaegi Runtime in sync with
// the .go source files on disk, and builds the frontend manifest from the JS files.
//
// Layout under Root:
//
//	<Root>/go/*.go           backend Go-source plugins (yaegi)
//	<Root>/frontend/*.js     frontend JS plugins (served via AssetURLBase)
type Manager struct {
	Root         string
	AssetURLBase string // e.g. "/api/plugin/asset"
	runtime      *Runtime
	registry     *Registry
	mu           sync.Mutex
	known        map[string]fileState // go-plugin path -> state
	loadErrors   map[string]string    // plugin name -> last load error (for the UI)
	stop         chan struct{}
	stopOnce     sync.Once
}

// Default is the process-wide manager, set during bootstrap. HTTP handlers read
// it; it may be nil if plugin support failed to initialise.
var Default *Manager

func NewManager(ctx context.Context, root, assetURLBase string) (*Manager, error) {
	_ = ctx // kept for signature/back-compat; the yaegi runtime needs no context
	reg := NewRegistry()
	return &Manager{
		Root:         root,
		AssetURLBase: strings.TrimRight(assetURLBase, "/"),
		runtime:      NewRuntime(reg),
		registry:     reg,
		known:        map[string]fileState{},
		loadErrors:   map[string]string{},
		stop:         make(chan struct{}),
	}, nil
}

func (m *Manager) Registry() *Registry { return m.registry }
func (m *Manager) Runtime() *Runtime   { return m.runtime }

func (m *Manager) goDir() string       { return filepath.Join(m.Root, "go") }
func (m *Manager) frontendDir() string { return filepath.Join(m.Root, "frontend") }

func nameFromPath(path, ext string) string {
	return strings.TrimSuffix(filepath.Base(path), ext)
}

// Sync reconciles loaded plugins with the .go files on disk: new or changed
// files are (re)loaded, deleted files are unloaded. Returns the names that were
// loaded/reloaded this pass. A missing directory is treated as empty.
func (m *Manager) Sync() ([]string, error) {
	entries, err := os.ReadDir(m.goDir())
	if err != nil {
		if os.IsNotExist(err) {
			m.unloadMissing(map[string]bool{})
			return nil, nil
		}
		return nil, err
	}

	present := map[string]bool{}
	var changed []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(m.goDir(), e.Name())
		info, statErr := e.Info()
		if statErr != nil {
			continue
		}
		present[path] = true

		m.mu.Lock()
		prev, ok := m.known[path]
		unchanged := ok && prev.mod.Equal(info.ModTime()) && prev.size == info.Size()
		m.mu.Unlock()
		if unchanged {
			continue
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			log.Warnf("[plugin] read %s: %v", path, readErr)
			continue
		}
		name := nameFromPath(path, ".go")
		// Runtime.Load drops the old version's hooks before re-subscribing, so it
		// is safe to call repeatedly for hot-reload.
		if err := m.runtime.Load(name, data); err != nil {
			log.Warnf("[plugin] load %s: %v", name, err)
			// Surface the error to the management UI, but still remember the
			// file state so a syntactically-broken plugin isn't retried every
			// tick — it retries when the file is edited (mtime/size changes).
			m.mu.Lock()
			m.loadErrors[name] = err.Error()
			m.known[path] = fileState{mod: info.ModTime(), size: info.Size()}
			m.mu.Unlock()
			continue
		}
		m.mu.Lock()
		delete(m.loadErrors, name)
		m.known[path] = fileState{mod: info.ModTime(), size: info.Size()}
		m.mu.Unlock()
		changed = append(changed, name)
		log.Infof("[plugin] loaded go plugin %q", name)
	}

	m.unloadMissing(present)
	return changed, nil
}

func (m *Manager) unloadMissing(present map[string]bool) {
	m.mu.Lock()
	var gone []string
	for path := range m.known {
		if !present[path] {
			gone = append(gone, path)
		}
	}
	for _, path := range gone {
		delete(m.known, path)
	}
	m.mu.Unlock()

	for _, path := range gone {
		name := nameFromPath(path, ".go")
		m.runtime.Unload(name) // also drops the plugin's hook subscriptions
		m.mu.Lock()
		delete(m.loadErrors, name)
		m.mu.Unlock()
		log.Infof("[plugin] unloaded go plugin %q", name)
	}
}

// FrontendManifest scans the frontend dir for .js plugins and returns the
// manifest the web UI fetches. A missing dir yields an empty manifest.
func (m *Manager) FrontendManifest() Manifest {
	manifest := Manifest{Plugins: []ManifestEntry{}}
	entries, err := os.ReadDir(m.frontendDir())
	if err != nil {
		return manifest
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		id := nameFromPath(e.Name(), ".js")
		manifest.Plugins = append(manifest.Plugins, ManifestEntry{
			ID:  id,
			URL: m.AssetURLBase + "/" + e.Name(),
		})
	}
	return manifest
}

// AssetPath resolves a requested asset filename to a path inside the frontend
// dir, rejecting any traversal outside it. Returns ("", false) when unsafe.
func (m *Manager) AssetPath(name string) (string, bool) {
	if name == "" || strings.Contains(name, "..") || filepath.IsAbs(name) {
		return "", false
	}
	full := filepath.Join(m.frontendDir(), filepath.Clean(name))
	rel, err := filepath.Rel(m.frontendDir(), full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

// Start runs Sync immediately and then every interval until Close, for hot-reload.
func (m *Manager) Start(interval time.Duration) {
	if _, err := m.Sync(); err != nil {
		log.Warnf("[plugin] initial sync: %v", err)
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-ticker.C:
				if _, err := m.Sync(); err != nil {
					log.Warnf("[plugin] sync: %v", err)
				}
			}
		}
	}()
}

// Close stops the watcher and tears down the runtime.
func (m *Manager) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	return m.runtime.Close()
}
