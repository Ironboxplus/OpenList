package stream

import (
	"context"
	"sync"
	"time"
)

// FileGate keeps background reads of a file (subtitle extraction, the library
// prefetch) off a storage that lets a file be downloaded over one connection at a
// time (driver.DownloadLimited), without ever slowing playback down.
//
// There are two kinds of holder, by file:
//   - playback: a proxy stream of the file. Playbacks never wait for one another
//     or for anything else, as before.
//   - background: one read at a time. It waits while a playback holds the file
//     and for a grace period after the last one let go (a player's requests come
//     with gaps, and a seek ends one before it starts the next). A playback that
//     starts while a background read is in flight cancels it at once; the read is
//     made again when the way is clear, so the work goes on from where it was.
type FileGate struct {
	// Grace is how long the file must have had no playback before background
	// reads resume.
	Grace time.Duration

	mu    sync.Mutex
	files map[string]*gateFile
}

type gateFile struct {
	playing   int
	idleSince time.Time
	bg        map[*bgRun]struct{}
	changed   chan struct{} // closed, and replaced, when playing changes
}

type bgRun struct {
	cancel    context.CancelFunc
	done      chan struct{}
	preempted bool
}

// DownloadGate is the process's gate.
var DownloadGate = &FileGate{Grace: 10 * time.Second}

// preemptWait bounds how long a starting playback waits for the background read it
// cancelled to let go of its connection.
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

// BeginPlayback registers a playback of key and returns the function that ends
// it. It does not wait for any other playback; it returns once the background
// reads of the file it cancelled have let go.
func (g *FileGate) BeginPlayback(key string) (end func()) {
	g.mu.Lock()
	f := g.file(key)
	f.playing++
	var wait []chan struct{}
	for r := range f.bg {
		r.preempted = true
		r.cancel()
		wait = append(wait, r.done)
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

// RunBackground runs one background read of key. It waits until no playback holds
// the file; fn gets a context that a starting playback cancels, and is then run
// again when the way is clear. An error from fn that the cancellation did not
// cause is returned as it is.
func (g *FileGate) RunBackground(ctx context.Context, key string, fn func(ctx context.Context) error) error {
	for {
		g.mu.Lock()
		f := g.file(key)
		var wait time.Duration
		if f.playing == 0 {
			wait = g.Grace - time.Since(f.idleSince)
		}
		if f.playing == 0 && wait <= 0 {
			cctx, cancel := context.WithCancel(ctx)
			run := &bgRun{cancel: cancel, done: make(chan struct{})}
			f.bg[run] = struct{}{}
			g.mu.Unlock()

			err := fn(cctx)

			g.mu.Lock()
			delete(f.bg, run)
			preempted := run.preempted
			close(run.done)
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
}

func (s *GatedSource) Size() int64 { return s.Inner.Size() }

func (s *GatedSource) ReadAt(ctx context.Context, off int64, n int) ([]byte, error) {
	var out []byte
	err := s.Gate.RunBackground(ctx, s.Key, func(c context.Context) error {
		var err error
		out, err = s.Inner.ReadAt(c, off, n)
		return err
	})
	return out, err
}
