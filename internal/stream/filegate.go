package stream

import (
	"context"
	"sync"
	"time"
)

// FileGate keeps background reads of a file (subtitle extraction, the library
// prefetch) within the number of simultaneous download connections a storage
// allows for one file (driver.DownloadLimited), without ever slowing playback.
// With N connections allowed, the file has N slots:
//   - a playback (a proxy stream of the file) takes a slot when it starts. It
//     never waits: for another playback, or for a background read. If every slot
//     is taken and a background read holds one, that read is cancelled at once
//     and its slot goes to the playback. If playbacks alone hold more than N
//     (a player that seeks with several ranges open) they all go ahead, as they
//     did before the gate: stalling playback is the worse mistake.
//   - a background read takes a slot only while one is free, and, after the file
//     was last played, once Grace has passed (a player's requests come with gaps
//     and a seek ends one before it starts the next). A read that was cancelled
//     by a playback is made again when a slot is free, so the work goes on from
//     where it was.
//
// A storage with no limit is not put through the gate at all.
type FileGate struct {
	// Grace is how long the file must have had no playback before a background
	// read may start.
	Grace time.Duration

	mu    sync.Mutex
	files map[string]*gateFile
}

type gateFile struct {
	playing   int
	idleSince time.Time
	bg        map[*bgRun]struct{}
	changed   chan struct{} // closed, and replaced, when a slot changes hands
}

type bgRun struct {
	cancel    context.CancelFunc
	done      chan struct{}
	preempted bool
}

// DownloadGate is the process's gate.
var DownloadGate = &FileGate{Grace: 10 * time.Second}

// preemptWait bounds how long a starting playback waits for the background reads
// it cancelled to let go of their connections.
const preemptWait = 2 * time.Second

func (g *FileGate) file(key string) *gateFile {
	if g.files == nil {
		g.files = map[string]*gateFile{}
	}
	f := g.files[key]
	if f == nil {
		f = &gateFile{bg: map[*bgRun]struct{}{}, changed: make(chan struct{})}
		g.files[key] = f
	}
	return f
}

func (f *gateFile) signal() {
	close(f.changed)
	f.changed = make(chan struct{})
}

// BeginPlayback registers a playback of key on a storage that allows limit
// connections per file, and returns the function that ends it. It returns once
// the background reads it had to cancel for a slot have let go.
func (g *FileGate) BeginPlayback(key string, limit int) (end func()) {
	g.mu.Lock()
	f := g.file(key)
	f.playing++
	var wait []chan struct{}
	if limit > 0 {
		excess := f.playing + len(f.bg) - limit
		for r := range f.bg {
			if excess <= 0 {
				break
			}
			r.preempted = true
			r.cancel()
			wait = append(wait, r.done)
			excess--
		}
	}
	f.signal()
	g.mu.Unlock()
	timeout := time.After(preemptWait)
	for _, d := range wait {
		select {
		case <-d:
		case <-timeout:
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			f.playing--
			if f.playing == 0 {
				f.idleSince = time.Now()
			}
			f.signal()
			if len(g.files) > 1024 {
				g.pruneLocked()
			}
		})
	}
}

// pruneLocked forgets files nobody has played or read for a while.
func (g *FileGate) pruneLocked() {
	for k, f := range g.files {
		if f.playing == 0 && len(f.bg) == 0 && time.Since(f.idleSince) > time.Hour {
			delete(g.files, k)
		}
	}
}

// Playing is true while a playback of key is open, or ended less than Grace ago.
func (g *FileGate) Playing(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.files[key]
	return f != nil && (f.playing > 0 || time.Since(f.idleSince) < g.Grace)
}

// RunBackground runs one background read of key, when a slot is free (limit is
// read at each try, so a setting can change under it; a limit of 0 or less runs
// fn at once). fn gets a context that a starting playback cancels when it needs
// the slot, and is then run again when one is free. An error from fn that the
// cancellation did not cause is returned as it is.
func (g *FileGate) RunBackground(ctx context.Context, key string, limit func() int, fn func(ctx context.Context) error) error {
	for {
		n := limit()
		if n <= 0 {
			return fn(ctx)
		}
		g.mu.Lock()
		f := g.file(key)
		var wait time.Duration
		if f.playing == 0 {
			wait = g.Grace - time.Since(f.idleSince)
		}
		if f.playing+len(f.bg) < n && wait <= 0 {
			cctx, cancel := context.WithCancel(ctx)
			run := &bgRun{cancel: cancel, done: make(chan struct{})}
			f.bg[run] = struct{}{}
			g.mu.Unlock()

			err := fn(cctx)

			g.mu.Lock()
			delete(f.bg, run)
			preempted := run.preempted
			close(run.done)
			f.signal()
			g.mu.Unlock()
			cancel()
			if err != nil && preempted && ctx.Err() == nil {
				continue
			}
			return err
		}
		changed := f.changed
		g.mu.Unlock()

		var timer <-chan time.Time
		if wait > 0 {
			timer = time.After(wait)
		}
		select {
		case <-changed:
		case <-timer:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// GatedSource is a reader of a file (subextract.Source, structurally) whose reads
// are background reads of Key.
type GatedSource struct {
	Inner interface {
		Size() int64
		ReadAt(ctx context.Context, off int64, n int) ([]byte, error)
	}
	Key  string
	Gate *FileGate
	// Limit is the connections per file the storage allows.
	Limit func() int
}

func (s *GatedSource) Size() int64 { return s.Inner.Size() }

func (s *GatedSource) ReadAt(ctx context.Context, off int64, n int) ([]byte, error) {
	var out []byte
	err := s.Gate.RunBackground(ctx, s.Key, s.Limit, func(c context.Context) error {
		var err error
		out, err = s.Inner.ReadAt(c, off, n)
		return err
	})
	return out, err
}
