package subextract

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// taskTimeout is how long one file may be read before the task is dropped. It is
// the time spent reading: a task parked behind a playback of the same file (see
// stream.FileGate) is not running out of it. A variable so that a test can
// shorten it, with timeoutCheck how often it is looked at.
var (
	taskTimeout  = 15 * time.Minute
	timeoutCheck = time.Second
)

// parker is a Source that knows how long it has spent waiting rather than
// reading (stream.GatedSource).
type parker interface{ Parked() time.Duration }

// File is one file the manager can extract from. Key identifies the file's
// revision (storage mount, path, size, modification time), so that a replaced
// file is not served from the cache of the one before it.
type File struct {
	Key string
	// Open makes the Source and a function that releases whatever backs it (the
	// link). It is called once per task.
	Open func(ctx context.Context) (Source, func(), error)
}

// Config is the manager's settings, read when they matter so that a change in
// the settings page takes effect without a restart.
type Config struct {
	// Dir is where finished tracks are kept.
	Dir string
	// MaxCacheBytes bounds the files under Dir.
	MaxCacheBytes func() int64
	// MaxFiles is how many files are read at once.
	MaxFiles func() int
}

// Result is what a request for a track gets.
type Result struct {
	Data   []byte
	Format string
	// Partial is true while the track is still being read: Data is complete
	// and valid but only covers part of the film, Done being the share read.
	Partial bool
	Done    float64
	// Gaps: the track was read to the end but some of it could not be decoded.
	// It is served, not cached; asking again reads it again.
	Gaps bool
}

type entry struct {
	ready  chan struct{}
	task   *Task
	err    error
	cancel context.CancelFunc
}

// Manager runs one task per file, however many requests ask for it.
type Manager struct {
	cfg   Config
	disk  *diskCache
	gate  limiter
	mu    sync.Mutex
	tasks map[string]*entry
	probe map[string][]TrackInfo
	// probing are the probes TracksWithin has running, by file.
	probing map[string]*probeCall
	order []string // probe keys, oldest first
}

const probeCacheMax = 256

// NewManager builds a manager. Zero settings get defaults: 512 MiB of cache,
// two files at a time.
func NewManager(cfg Config) *Manager {
	if cfg.MaxCacheBytes == nil {
		cfg.MaxCacheBytes = func() int64 { return 512 << 20 }
	}
	if cfg.MaxFiles == nil {
		cfg.MaxFiles = func() int { return 2 }
	}
	return &Manager{
		cfg:     cfg,
		disk:    &diskCache{dir: filepath.Clean(cfg.Dir), max: cfg.MaxCacheBytes},
		gate:    limiter{wake: make(chan struct{})},
		tasks:   map[string]*entry{},
		probe:   map[string][]TrackInfo{},
		probing: map[string]*probeCall{},
	}
}

// Tracks lists the subtitle tracks of the file, remembering the answer.
func (m *Manager) Tracks(ctx context.Context, f File) ([]TrackInfo, error) {
	m.mu.Lock()
	if t, ok := m.probe[f.Key]; ok {
		m.mu.Unlock()
		return t, nil
	}
	if e := m.tasks[f.Key]; e != nil && e.task != nil {
		tracks := e.task.Tracks()
		m.mu.Unlock()
		return tracks, nil
	}
	m.mu.Unlock()

	src, closeFn, err := f.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	tracks, err := Probe(ctx, src)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if _, ok := m.probe[f.Key]; !ok {
		m.probe[f.Key] = tracks
		m.order = append(m.order, f.Key)
		if len(m.order) > probeCacheMax {
			delete(m.probe, m.order[0])
			m.order = m.order[1:]
		}
	}
	m.mu.Unlock()
	return tracks, nil
}

// probeCall is a probe running in the background for TracksWithin.
type probeCall struct {
	done   chan struct{}
	tracks []TrackInfo
	err    error
}

// probeBudget is how long a background probe may take.
const probeBudget = 2 * time.Minute

// TracksWithin is Tracks for a caller that cannot wait: when the file's tracks
// are not known yet and the probe takes longer than wait, it returns ready=false
// and the probe carries on in the background, so that the next call has them.
// Calls for the same file share one probe.
func (m *Manager) TracksWithin(ctx context.Context, f File, wait time.Duration) (tracks []TrackInfo, ready bool, err error) {
	m.mu.Lock()
	_, known := m.probe[f.Key]
	if e := m.tasks[f.Key]; e != nil && e.task != nil {
		known = true
	}
	m.mu.Unlock()
	if known {
		tracks, err = m.Tracks(ctx, f)
		return tracks, err == nil, err
	}

	m.mu.Lock()
	call := m.probing[f.Key]
	if call == nil {
		call = &probeCall{done: make(chan struct{})}
		m.probing[f.Key] = call
		go func() {
			bg, cancel := context.WithTimeout(context.Background(), probeBudget)
			defer cancel()
			call.tracks, call.err = m.Tracks(bg, f)
			m.mu.Lock()
			delete(m.probing, f.Key)
			m.mu.Unlock()
			close(call.done)
		}()
	}
	m.mu.Unlock()
	select {
	case <-call.done:
		return call.tracks, call.err == nil, call.err
	case <-time.After(wait):
		return nil, false, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

// Cached reports whether a track of the file is already a finished file on disk.
func (m *Manager) Cached(f File, order int) bool {
	_, _, ok := m.disk.Get(f.Key, order)
	return ok
}

// Fill reads the given tracks of the file to the end, in the background's
// place: behind any track a viewer asked for, and it returns when they are all
// on disk, one fails, or ctx ends. Whatever ctx's end leaves unread is dropped
// unless a viewer has asked for it meanwhile.
func (m *Manager) Fill(ctx context.Context, f File, orders []int) error {
	var todo []int
	for _, o := range orders {
		if !m.Cached(f, o) {
			todo = append(todo, o)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	var task *Task
	for attempt := 0; ; attempt++ {
		var err error
		if task, err = m.taskFor(f); err != nil {
			return err
		}
		closed := false
		for _, o := range todo {
			if err := task.Want(o, false, -1); errors.Is(err, errTaskClosed) {
				closed = true
				break
			} else if err != nil {
				return err
			}
		}
		if !closed {
			break
		}
		if attempt > 0 {
			return errTaskClosed
		}
	}
	defer m.abandon(f.Key, task)
	for {
		idle := task.Idle()
		complete := true
		for _, o := range todo {
			_, _, _, c, err := task.Snapshot(o)
			if err != nil {
				return err
			}
			complete = complete && c
		}
		if complete {
			return nil
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// abandon stops reading a file nobody is watching any more.
func (m *Manager) abandon(key string, task *Task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.tasks[key]; e != nil && e.task == task && !task.Hot() && !task.Done() {
		e.cancel()
	}
}

// Get returns a track, waiting up to wait for it to have something to show. at
// is the playback position in milliseconds: reading starts there.
func (m *Manager) Get(ctx context.Context, f File, order int, atMs int64, wait time.Duration) (Result, error) {
	if data, format, ok := m.disk.Get(f.Key, order); ok {
		return Result{Data: data, Format: format, Done: 1}, nil
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		task, err := m.taskFor(f)
		if err != nil {
			return Result{}, err
		}
		if err := task.Want(order, true, atMs); errors.Is(err, errTaskClosed) {
			lastErr = err
			continue
		} else if err != nil {
			return Result{}, err
		}
		wctx, cancel := context.WithTimeout(ctx, wait)
		task.Wait(wctx, order)
		cancel()
		data, format, done, complete, err := task.Snapshot(order)
		if err != nil && !complete {
			return Result{}, err
		}
		return Result{Data: data, Format: format, Partial: !complete, Done: done, Gaps: complete && task.Gaps(order)}, nil
	}
	return Result{}, lastErr
}

// Busy is true while a track a viewer asked for is being read: work that can
// wait (reading a whole library ahead) waits for it.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.tasks {
		if e.task != nil && e.task.Hot() {
			return true
		}
	}
	return false
}

// taskFor returns the file's running task, making it if there is none. Only
// one caller makes it; the others wait for the same result.
func (m *Manager) taskFor(f File) (*Task, error) {
	m.mu.Lock()
	if e, ok := m.tasks[f.Key]; ok {
		m.mu.Unlock()
		<-e.ready
		return e.task, e.err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &entry{ready: make(chan struct{}), cancel: cancel}
	m.tasks[f.Key] = e
	m.mu.Unlock()

	// The deadline: taskTimeout of reading, from now.
	var opened atomic.Value // holds struct{ Source }
	go func() {
		start := time.Now()
		tick := time.NewTicker(timeoutCheck)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				var parked time.Duration
				if o, ok := opened.Load().(struct{ Source }); ok {
					if p, ok := o.Source.(parker); ok {
						parked = p.Parked()
					}
				}
				if time.Since(start)-parked > taskTimeout {
					cancel()
					return
				}
			}
		}
	}()

	var closeFn func()
	fail := func(err error) (*Task, error) {
		cancel()
		if closeFn != nil {
			closeFn()
		}
		m.mu.Lock()
		delete(m.tasks, f.Key)
		m.mu.Unlock()
		e.err = err
		close(e.ready)
		return nil, err
	}
	src, closeFn, err := f.Open(ctx)
	if err != nil {
		return fail(err)
	}
	opened.Store(struct{ Source }{src})
	gate := func(ctx context.Context) (func(), error) { return m.gate.acquire(ctx, m.cfg.MaxFiles) }
	onDone := func(order int, format string, data []byte) {
		if err := m.disk.Put(f.Key, order, format, data); err != nil {
			log.Warnf("subextract: cache write: %v", err)
		}
	}
	task, err := newTask(ctx, src, gate, onDone)
	if err != nil {
		return fail(err)
	}
	e.task = task
	close(e.ready)
	// Release the file when the work is done: the task and its link do not
	// outlive the last track asked for.
	go func() {
		defer cancel()
		defer closeFn()
		for {
			select {
			case <-task.Idle():
				m.mu.Lock()
				if task.Done() {
					task.Close()
					delete(m.tasks, f.Key)
					m.mu.Unlock()
					return
				}
				m.mu.Unlock()
				// Asked again while this ran down: the loop restarted.
				time.Sleep(10 * time.Millisecond)
			case <-ctx.Done():
				task.Close()
				m.mu.Lock()
				delete(m.tasks, f.Key)
				m.mu.Unlock()
				return
			}
		}
	}()
	return task, nil
}

// limiter is a counting semaphore whose limit is read at each attempt.
type limiter struct {
	mu     sync.Mutex
	active int
	wake   chan struct{}
}

func (l *limiter) acquire(ctx context.Context, max func() int) (func(), error) {
	for {
		l.mu.Lock()
		if limit := max(); l.active < limit || limit <= 0 && l.active == 0 {
			l.active++
			l.mu.Unlock()
			return l.release, nil
		}
		ch := l.wake
		l.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (l *limiter) release() {
	l.mu.Lock()
	l.active--
	close(l.wake)
	l.wake = make(chan struct{})
	l.mu.Unlock()
}
