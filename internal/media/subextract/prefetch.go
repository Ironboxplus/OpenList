package subextract

import (
	"context"
	"sync"
	"time"
)

// Target is a file the prefetch looks at: how to read it, and which of its
// subtitle tracks something else (the storage provider's own files) already
// stands in for. Cover is only called for a file that is not done yet, because
// asking a provider costs a request of its own; nil means nothing covers any.
type Target struct {
	File  File
	Cover func(ctx context.Context) (func(TrackInfo) bool, error)
}

// PrefetchState is what a pass reports after each file.
type PrefetchState struct {
	// Files looked at so far; Tracks put on disk by this pass; Failed files.
	Files, Tracks, Failed int
	// Current is the file being worked on, "" between files.
	Current string
	Err     error
}

// Prefetcher fills the cache for a list of files in the background, one file at
// a time and behind any track a viewer asks for.
type Prefetcher struct {
	m *Manager
	// Open resolves a path to its Target; nil, nil is a file with nothing to
	// extract (not a container with subtitle tracks, or no other source to play).
	Open func(ctx context.Context, path string) (*Target, error)

	busyWait time.Duration
	mu       sync.Mutex
	done     map[string]bool
}

func NewPrefetcher(m *Manager, open func(ctx context.Context, path string) (*Target, error)) *Prefetcher {
	return &Prefetcher{m: m, Open: open, busyWait: 2 * time.Second, done: map[string]bool{}}
}

// Run goes through paths. A file whose tracks were all dealt with in an earlier
// pass of this process (same size and modification time) costs no request to
// the storage's data: only a file that failed, or changed, is looked at again.
func (p *Prefetcher) Run(ctx context.Context, paths []string, report func(PrefetchState)) {
	var st PrefetchState
	for _, path := range paths {
		for p.m.Busy() && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(p.busyWait):
			}
		}
		if ctx.Err() != nil {
			break
		}
		st.Current = path
		report(st)
		n, err := p.one(ctx, path)
		st.Files++
		st.Tracks += n
		if err != nil && ctx.Err() == nil {
			st.Failed++
			st.Err = err
		}
	}
	st.Current = ""
	report(st)
}

func (p *Prefetcher) one(ctx context.Context, path string) (int, error) {
	target, err := p.Open(ctx, path)
	if err != nil || target == nil {
		return 0, err
	}
	p.mu.Lock()
	done := p.done[target.File.Key]
	p.mu.Unlock()
	if done {
		return 0, nil
	}
	tracks, err := p.m.Tracks(ctx, target.File)
	if err != nil {
		return 0, err
	}
	covered := func(TrackInfo) bool { return false }
	if target.Cover != nil {
		if covered, err = target.Cover(ctx); err != nil {
			return 0, err
		}
	}
	var todo []int
	for _, t := range tracks {
		if t.Supported && !covered(t) && !p.m.Cached(target.File, t.Order) {
			todo = append(todo, t.Order)
		}
	}
	if err := p.m.Fill(ctx, target.File, todo); err != nil {
		return 0, err
	}
	// A track that came out with gaps is not cached and would be read again by
	// every pass for the same gaps: it is as done as it will get.
	p.mu.Lock()
	p.done[target.File.Key] = true
	p.mu.Unlock()
	return len(todo), nil
}
