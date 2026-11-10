package subextract

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

const (
	// mergeGap is how far apart two units of work may lie and still be one
	// read: the bytes between them are read and thrown away, which costs less
	// than a second round trip.
	mergeGap = 64 << 10
	// groupSpan caps one merged read.
	groupSpan = 4 << 20
	// pivotLookback: extraction starts this far before the playback position,
	// so a cue that began just before it and is still on screen is there.
	pivotLookback = 10_000
)

// groupItems caps the units in one merged read (a variable so that a test can
// make small files take several).
var groupItems = 256

// SourceError is a failure of the Source itself (as opposed to a block that
// cannot be decoded): the task stops on it.
type SourceError struct{ Err error }

func (e *SourceError) Error() string { return e.Err.Error() }
func (e *SourceError) Unwrap() error { return e.Err }

// Open reads the structure of a file: Matroska or MP4. With withCues the
// Matroska index of the subtitle blocks is read too, which probing the track
// list does not need.
func open(ctx context.Context, src Source, withCues bool) (container, error) {
	m, err := openMKV(ctx, src, withCues)
	if err == nil {
		return m, nil
	}
	if !errors.Is(err, ErrUnsupportedContainer) {
		return nil, err
	}
	p, err := openMP4(ctx, src)
	if err != nil {
		return nil, ErrUnsupportedContainer
	}
	return p, nil
}

// Probe lists the subtitle tracks of the file.
func Probe(ctx context.Context, src Source) ([]TrackInfo, error) {
	c, err := open(ctx, src, false)
	if err != nil {
		return nil, err
	}
	return c.Tracks(), nil
}

type trackState struct {
	info   TrackInfo
	header []byte
	items  []Item
	taken  []bool
	nTaken int
	cues   []Cue
	hot    bool
	err    error
	// gaps is set when a unit of the track could not be read or decoded: what
	// there is can be served, but it is not the whole track and is never kept.
	gaps bool
	// rendered is the file for the cues so far, kept until more arrive.
	rendered   []byte
	renderedAt int
}

func (s *trackState) done() bool { return s.nTaken == len(s.items) }

type ref struct{ track, idx int }

// Task extracts tracks of one file. It reads the file strictly one range at a
// time, nearest the playback position first and then wrapping round, for all
// the tracks wanted at once, so that the blocks of one cluster are read once.
type Task struct {
	src Source
	c   container

	mu      sync.Mutex
	tracks  map[int]*trackState
	pivot   int64 // ms
	queue   []ref
	qi      int
	dirty   bool
	running bool
	changed chan struct{} // closed and replaced whenever something is delivered
	ctx     context.Context
	gate    func(context.Context) (func(), error)
	// onDone is called, outside the lock, with a track's finished file.
	onDone func(order int, format string, data []byte)
	// persisting counts the finished files whose onDone has not returned: a
	// track is complete in Snapshot before its file is written.
	persisting sync.WaitGroup
	err    error
	closed bool
	// idle is closed each time a run of the reading loop ends.
	idle chan struct{}
}

var errTaskClosed = errors.New("subextract: task closed")

// newTask opens file structure with its Cues. gate, when set, is held for as
// long as the task reads (the manager's limit on files read at once).
func newTask(ctx context.Context, src Source, gate func(context.Context) (func(), error), onDone func(int, string, []byte)) (*Task, error) {
	c, err := open(ctx, src, true)
	if err != nil {
		return nil, err
	}
	return &Task{
		src: src, c: c, ctx: ctx, gate: gate, onDone: onDone,
		tracks: map[int]*trackState{}, changed: make(chan struct{}), idle: make(chan struct{}),
	}, nil
}

// Tracks lists the file's subtitle tracks.
func (t *Task) Tracks() []TrackInfo { return t.c.Tracks() }

// Want asks for a track, with the playback position (ms) extraction should
// start from. hot marks a track a viewer asked for, which is read before the
// tracks that are only being prepared.
func (t *Task) Want(order int, hot bool, atMs int64) error {
	infos := t.c.Tracks()
	if order < 0 || order >= len(infos) {
		return fmt.Errorf("subextract: no subtitle track %d", order)
	}
	if !infos[order].Supported {
		return fmt.Errorf("subextract: track %d (%s) cannot be extracted", order, infos[order].Codec)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errTaskClosed
	}
	st := t.tracks[order]
	if st == nil {
		items, err := t.c.Items(order)
		if err != nil {
			return err
		}
		for i := range items {
			items[i].idx = i
		}
		st = &trackState{info: infos[order], header: t.c.Header(order), items: items, taken: make([]bool, len(items))}
		t.tracks[order] = st
		t.dirty = true
	}
	if hot && !st.hot {
		st.hot = true
		t.dirty = true
	}
	if atMs >= 0 && atMs != t.pivot {
		t.pivot = atMs
		t.dirty = true
	}
	if !t.running && !t.finishedLocked() {
		t.running = true
		t.idle = make(chan struct{})
		go t.run()
	}
	return nil
}

// Idle returns a channel that is closed when the reading loop next stops (or
// already has).
func (t *Task) Idle() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.idle
}

// Hot is true while a track a viewer asked for is still being read.
func (t *Task) Hot() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.running {
		return false
	}
	for _, st := range t.tracks {
		if st.hot && !st.done() {
			return true
		}
	}
	return false
}

// Close refuses further requests; the context passed to newTask stops the read.
func (t *Task) Close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

func (t *Task) finishedLocked() bool {
	for _, st := range t.tracks {
		if !st.done() && st.err == nil {
			return false
		}
	}
	return true
}

// Snapshot is a track's file as far as it is known. done is the share of the
// track's units read; complete is true when all of them were.
func (t *Task) Snapshot(order int) (data []byte, format string, done float64, complete bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.tracks[order]
	if st == nil {
		return nil, "", 0, false, fmt.Errorf("subextract: track %d was not asked for", order)
	}
	if st.rendered == nil || st.renderedAt != len(st.cues) {
		st.rendered = Render(st.info.Format, st.header, st.cues)
		st.renderedAt = len(st.cues)
	}
	done = 1
	if n := len(st.items); n > 0 {
		done = float64(st.nTaken) / float64(n)
	}
	return st.rendered, st.info.Format, done, st.done(), st.err
}

// Wait blocks until the track has something to show, is complete, has failed,
// or the timeout (the context) ends.
func (t *Task) Wait(ctx context.Context, order int) {
	for {
		t.mu.Lock()
		st := t.tracks[order]
		ready := st == nil || len(st.cues) > 0 || st.done() || st.err != nil || t.err != nil || !t.running
		ch := t.changed
		t.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
}

// Gaps is true when the track is whole except for units that could not be read
// or decoded.
func (t *Task) Gaps(order int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.tracks[order]
	return st != nil && st.gaps
}

// Done is true once nothing is left to read for the tracks asked for.
func (t *Task) Done() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.running && t.finishedLocked()
}

// rebuildLocked orders the units still to read: hot tracks first, from the
// playback position to the end and then from the start to it, by time.
func (t *Task) rebuildLocked() {
	t.queue = t.queue[:0]
	for order, st := range t.tracks {
		for i := range st.items {
			if !st.taken[i] {
				t.queue = append(t.queue, ref{order, i})
			}
		}
	}
	from := t.pivot - pivotLookback
	key := func(r ref) (int, int, int64, int64) {
		st := t.tracks[r.track]
		it := st.items[r.idx]
		cold, wrap := 1, 1
		if st.hot {
			cold = 0
		}
		if it.Time >= from {
			wrap = 0
		}
		return cold, wrap, it.Time, it.Pos
	}
	sort.SliceStable(t.queue, func(i, j int) bool {
		ac, aw, at, ap := key(t.queue[i])
		bc, bw, bt, bp := key(t.queue[j])
		switch {
		case ac != bc:
			return ac < bc
		case aw != bw:
			return aw < bw
		case at != bt:
			return at < bt
		}
		return ap < bp
	})
	t.qi, t.dirty = 0, false
}

// nextGroupLocked takes the next run of nearby units off the queue.
func (t *Task) nextGroupLocked() []Item {
	if t.dirty {
		t.rebuildLocked()
	}
	var group []Item
	var prev Item
	for ; t.qi < len(t.queue); t.qi++ {
		r := t.queue[t.qi]
		st := t.tracks[r.track]
		if st.taken[r.idx] {
			continue
		}
		it := st.items[r.idx]
		if len(group) > 0 {
			end := prev.Pos + prev.Size
			if it.Pos < prev.Pos || it.Pos-end > mergeGap || it.Pos-group[0].Pos > groupSpan || len(group) >= groupItems {
				break
			}
		}
		group = append(group, it)
		prev = it
	}
	return group
}

func (t *Task) run() {
	var release func()
	if t.gate != nil {
		var err error
		if release, err = t.gate(t.ctx); err != nil {
			t.fail(err)
			return
		}
		defer release()
	}
	w := newWindow(t.src, 0)
	for {
		t.mu.Lock()
		group := t.nextGroupLocked()
		if len(group) == 0 {
			// Anything asked for while the last group was read has set dirty:
			// go round for the rebuild. Otherwise this is the end.
			if t.dirty {
				t.mu.Unlock()
				continue
			}
			t.running = false
			close(t.idle)
			t.signalLocked()
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()

		cues, failed, err := t.c.Fetch(t.ctx, w, group)
		if err != nil || t.ctx.Err() != nil {
			if err == nil {
				err = t.ctx.Err()
			}
			t.fail(err)
			return
		}

		var finished []int
		t.mu.Lock()
		for _, it := range failed {
			if st := t.tracks[it.Track]; st != nil {
				st.gaps = true
			}
		}
		for _, c := range cues {
			if st := t.tracks[c.Track]; st != nil {
				st.cues = append(st.cues, c)
			}
		}
		for _, it := range group {
			st := t.tracks[it.Track]
			if st == nil {
				continue
			}
			if !st.taken[it.idx] {
				st.taken[it.idx] = true
				st.nTaken++
				if st.done() && !st.gaps {
					finished = append(finished, it.Track)
				}
			}
		}
		if t.onDone != nil {
			t.persisting.Add(len(finished))
		}
		t.signalLocked()
		t.mu.Unlock()
		if t.onDone != nil {
			for _, order := range finished {
				data, format, _, _, _ := t.Snapshot(order)
				t.onDone(order, format, data)
				t.persisting.Done()
			}
		}
	}
}

func (t *Task) signalLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

func (t *Task) fail(err error) {
	t.mu.Lock()
	t.err = err
	t.running = false
	close(t.idle)
	for _, st := range t.tracks {
		if !st.done() {
			st.err = err
		}
	}
	t.signalLocked()
	t.mu.Unlock()
}

// Persisted waits until the files of the tracks that have finished are written
// (onDone has returned for each of them).
func (t *Task) Persisted() { t.persisting.Wait() }
