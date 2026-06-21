package op

import "sync"

// StorageLoadState is the lifecycle state of a single storage during startup load.
type StorageLoadState string

const (
	LoadPending StorageLoadState = "pending"
	LoadLoading StorageLoadState = "loading"
	LoadLoaded  StorageLoadState = "loaded"
	LoadFailed  StorageLoadState = "failed"
)

func (s StorageLoadState) terminal() bool {
	return s == LoadLoaded || s == LoadFailed
}

// LoadTarget is the minimal description of a storage to be loaded. Kept free of
// model.Storage so the tracker stays trivially testable.
type LoadTarget struct {
	MountPath string
	Driver    string
}

// StorageLoadInfo is the public, snapshot-safe view of one storage's load state.
type StorageLoadInfo struct {
	MountPath string           `json:"mount_path"`
	Driver    string           `json:"driver"`
	State     StorageLoadState `json:"state"`
	Error     string           `json:"error,omitempty"`
}

// ProgressSnapshot is an immutable view of the overall loading progress, safe to
// serialise to JSON and hand to the frontend status bar.
type ProgressSnapshot struct {
	Total    int               `json:"total"`
	Pending  int               `json:"pending"`
	Loading  int               `json:"loading"`
	Loaded   int               `json:"loaded"`
	Failed   int               `json:"failed"`
	Finished bool              `json:"finished"`
	Items    []StorageLoadInfo `json:"items"`
}

// LoadingProgress tracks per-storage load state during startup. All methods are
// safe for concurrent use, so storages may be loaded in parallel while the HTTP
// API polls Snapshot().
type LoadingProgress struct {
	mu      sync.RWMutex
	items   map[string]*StorageLoadInfo
	order   []string
	started bool
}

func NewLoadingProgress() *LoadingProgress {
	return &LoadingProgress{items: map[string]*StorageLoadInfo{}}
}

// Begin (re)initialises the tracker with all targets in the pending state.
func (p *LoadingProgress) Begin(targets []LoadTarget) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = make(map[string]*StorageLoadInfo, len(targets))
	p.order = make([]string, 0, len(targets))
	p.started = true
	for _, t := range targets {
		if _, ok := p.items[t.MountPath]; ok {
			continue
		}
		p.items[t.MountPath] = &StorageLoadInfo{
			MountPath: t.MountPath,
			Driver:    t.Driver,
			State:     LoadPending,
		}
		p.order = append(p.order, t.MountPath)
	}
}

// SetState updates the state (and optional error) of one storage. Unknown mount
// paths are ignored so a late rename can't corrupt the counts.
func (p *LoadingProgress) SetState(mountPath string, state StorageLoadState, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	info, ok := p.items[mountPath]
	if !ok {
		return
	}
	info.State = state
	info.Error = errMsg
}

// Snapshot returns a deep copy of the current progress.
func (p *LoadingProgress) Snapshot() ProgressSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	snap := ProgressSnapshot{
		Total: len(p.order),
		Items: make([]StorageLoadInfo, 0, len(p.order)),
	}
	terminal := 0
	for _, mp := range p.order {
		info := p.items[mp]
		switch info.State {
		case LoadPending:
			snap.Pending++
		case LoadLoading:
			snap.Loading++
		case LoadLoaded:
			snap.Loaded++
		case LoadFailed:
			snap.Failed++
		}
		if info.State.terminal() {
			terminal++
		}
		snap.Items = append(snap.Items, *info)
	}
	snap.Finished = p.started && terminal == len(p.order)
	return snap
}

// StorageLoadProgress is the global tracker populated by bootstrap.LoadStorages
// and read by the storage loading-status API.
var StorageLoadProgress = NewLoadingProgress()
