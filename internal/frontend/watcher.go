package frontend

import (
	"context"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

const defaultCheckInterval = 30 * time.Minute

// Watcher periodically checks for new frontend versions and fetches them.
type Watcher struct {
	interval    time.Duration
	stopCh      chan struct{}
	stopped     bool
	mu          sync.Mutex
	onUpdated   func()
}

// NewWatcher creates a new frontend watcher.
// onUpdated is called when a new version is fetched (used to reload static files).
func NewWatcher(onUpdated func()) *Watcher {
	return &Watcher{
		interval:  defaultCheckInterval,
		stopCh:    make(chan struct{}),
		onUpdated: onUpdated,
	}
}

// SetInterval changes the check interval. Must be called before Start.
func (w *Watcher) SetInterval(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.interval = d
}

// Start begins the periodic check loop in a background goroutine.
func (w *Watcher) Start() {
	w.mu.Lock()
	interval := w.interval
	w.mu.Unlock()

	go func() {
		utils.Log.Infof("[frontend] watcher started, checking every %s", interval)
		// Check immediately on start, then periodically
		w.check()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-w.stopCh:
				utils.Log.Infof("[frontend] watcher stopped")
				return
			case <-ticker.C:
				w.check()
			}
		}
	}()
}

// Stop signals the watcher to stop.
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.stopped = true
	close(w.stopCh)
}

func (w *Watcher) check() {
	if !shouldAutoFetch() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := FetchFromRolling(ctx)
	if err != nil {
		utils.Log.Warnf("[frontend] watcher check failed: %v", err)
		return
	}

	if result.Downloaded {
		utils.Log.Infof("[frontend] watcher fetched new version: %s", result.Version)
		if w.onUpdated != nil {
			w.onUpdated()
		}
	}
}

// globalWatcher is the singleton watcher instance
var (
	globalWatcher *Watcher
	watcherMu     sync.Mutex
)

// StartWatcher starts the global frontend watcher.
func StartWatcher(onUpdated func()) {
	watcherMu.Lock()
	defer watcherMu.Unlock()
	if globalWatcher != nil {
		return
	}
	globalWatcher = NewWatcher(onUpdated)
	globalWatcher.Start()
}

// StopWatcher stops the global frontend watcher.
func StopWatcher() {
	watcherMu.Lock()
	defer watcherMu.Unlock()
	if globalWatcher != nil {
		globalWatcher.Stop()
		globalWatcher = nil
	}
}
