package subextract

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const assHeader = "[Script Info]\nScriptType: v4.00+\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n"

// assFile is a Matroska file with one ASS track of n events, in which the
// events listed in broken are not ASS events at all (no ReadOrder), so that
// their blocks cannot be decoded.
func assFile(n int, broken ...int) []byte {
	var blocks []bBlock
	for i := 0; i < n; i++ {
		text := fmt.Sprintf("%d,0,Default,,0,0,0,,line %d", i, i)
		for _, b := range broken {
			if b == i {
				text = "no read order here"
			}
		}
		blocks = append(blocks, bBlock{track: 1, ms: int64(i)*1000 + 100, dur: 800, frames: [][]byte{[]byte(text)}})
	}
	return buildMKV([]bTrack{{num: 1, codec: "S_TEXT/ASS", lang: "eng", private: []byte(assHeader)}}, blocks, 4000, true)
}

func TestAnUndecodableBlockIsAGapNotAWholeLostGroupAndIsNeverCached(t *testing.T) {
	data := assFile(8, 3)
	s := serve(t, data)
	m := NewManager(Config{Dir: t.TempDir()})
	file := File{Key: "gaps|/a.mkv|1|1", Open: func(ctx context.Context) (Source, func(), error) {
		src, err := NewLinkSource(linkTo(s), int64(len(data)))
		return src, func() {}, err
	}}
	get := func() Result {
		t.Helper()
		for {
			r, err := m.Get(context.Background(), file, 0, 0, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Partial {
				return r
			}
		}
	}

	r := get()
	// the seven events that decode are served, the one that does not is a gap
	if n := strings.Count(string(r.Data), "Dialogue:"); n != 7 {
		t.Fatalf("%d events served, want 7:\n%s", n, r.Data)
	}
	if !r.Gaps {
		t.Error("the result does not say it has gaps")
	}
	time.Sleep(100 * time.Millisecond)
	if m.Cached(file, 0) {
		t.Fatal("a track with a gap was written to the disk cache")
	}
	// so asking again reads the file again
	before, _, _ := s.stats()
	if r = get(); !r.Gaps {
		t.Error("the second answer has no gaps flag")
	}
	if after, _, _ := s.stats(); after == before {
		t.Error("the second request did not read the file again")
	}
}

func TestAWholeTrackIsCachedAndReportsNoGaps(t *testing.T) {
	data := assFile(6)
	s := serve(t, data)
	m := NewManager(Config{Dir: t.TempDir()})
	file := File{Key: "whole|/a.mkv|1|1", Open: func(ctx context.Context) (Source, func(), error) {
		src, err := NewLinkSource(linkTo(s), int64(len(data)))
		return src, func() {}, err
	}}
	if err := m.Fill(context.Background(), file, []int{0}); err != nil {
		t.Fatal(err)
	}
	if !m.Cached(file, 0) {
		t.Fatal("a whole track is not cached")
	}
	r, err := m.Get(context.Background(), file, 0, 0, time.Second)
	if err != nil || r.Gaps || r.Partial {
		t.Fatalf("err=%v gaps=%v partial=%v", err, r.Gaps, r.Partial)
	}
}

// A small library: the real five-track film (one of whose tracks a provider
// already supplies), a file of ASS events, and one with a block that cannot be
// decoded.
func TestPrefetchPassFillsTheLibraryAndTheNextPassReadsNothing(t *testing.T) {
	film := makeFilm(t)
	type entry struct {
		data []byte
		s    *served
	}
	lib := map[string]*entry{}
	for name, data := range map[string][]byte{
		"/film.mkv":   film.data,
		"/plain.mkv":  assFile(10),
		"/broken.mkv": assFile(10, 4),
	} {
		lib[name] = &entry{data: data, s: serve(t, data)}
	}
	open := func(ctx context.Context, path string) (*Target, error) {
		e := lib[path]
		if e == nil {
			return nil, fmt.Errorf("no such file %s", path)
		}
		tg := &Target{File: File{
			Key: "lib|" + path + "|1|1",
			Open: func(ctx context.Context) (Source, func(), error) {
				src, err := NewLinkSource(linkTo(e.s), int64(len(e.data)))
				return src, func() {}, err
			},
		}}
		if path == "/film.mkv" {
			tg.Cover = func(context.Context) (func(TrackInfo) bool, error) {
				return func(ti TrackInfo) bool { return ti.Order == 0 }, nil
			}
		}
		return tg, nil
	}
	m := NewManager(Config{Dir: t.TempDir()})
	p := NewPrefetcher(m, open)
	p.busyWait = time.Millisecond
	paths := []string{"/film.mkv", "/plain.mkv", "/broken.mkv"}
	requests := func() (n int) {
		for _, e := range lib {
			r, _, _ := e.s.stats()
			n += r
		}
		return n
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var st PrefetchState
	p.Run(ctx, paths, func(s PrefetchState) { st = s })
	if st.Files != 3 || st.Failed != 0 || st.Current != "" {
		t.Fatalf("after the first pass: %+v", st)
	}
	// film: the four tracks the provider does not supply; plain: 1; broken: 1
	if st.Tracks != 4+1+1 {
		t.Errorf("%d tracks extracted, want 6", st.Tracks)
	}
	cached := func(path string, order int) bool { return m.Cached(File{Key: "lib|" + path + "|1|1"}, order) }
	for order := 1; order <= 4; order++ {
		if !cached("/film.mkv", order) {
			t.Errorf("film track %d is not on disk", order)
		}
	}
	if cached("/film.mkv", 0) {
		t.Error("film track 0 is covered by the provider and must not be read")
	}
	if !cached("/plain.mkv", 0) {
		t.Error("plain.mkv track 0 is not on disk")
	}
	if cached("/broken.mkv", 0) {
		t.Error("the track with a gap was cached")
	}
	for path, e := range lib {
		if _, _, maxIn := e.s.stats(); maxIn != 1 {
			t.Errorf("%s: %d requests in flight at once", path, maxIn)
		}
	}

	// The second pass: every file is done (the one with a gap included, whose
	// gap would only be found again), so nothing is asked of any of them.
	before := requests()
	p.Run(ctx, paths, func(s PrefetchState) { st = s })
	if after := requests(); after != before {
		t.Errorf("the second pass made %d range request(s)", after-before)
	}
	if st.Files != 3 || st.Tracks != 0 || st.Failed != 0 {
		t.Errorf("after the second pass: %+v", st)
	}
}

func TestPrefetchLooksAgainAtAFileThatFailed(t *testing.T) {
	data := assFile(5)
	s := serve(t, data)
	calls := 0
	m := NewManager(Config{Dir: t.TempDir()})
	p := NewPrefetcher(m, func(ctx context.Context, path string) (*Target, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("the storage is down")
		}
		return &Target{File: File{Key: "retry|1", Open: func(ctx context.Context) (Source, func(), error) {
			src, err := NewLinkSource(linkTo(s), int64(len(data)))
			return src, func() {}, err
		}}}, nil
	})
	p.busyWait = time.Millisecond
	var st PrefetchState
	p.Run(context.Background(), []string{"/a.mkv"}, func(s PrefetchState) { st = s })
	if st.Failed != 1 || st.Err == nil {
		t.Fatalf("first pass: %+v", st)
	}
	p.Run(context.Background(), []string{"/a.mkv"}, func(s PrefetchState) { st = s })
	if st.Failed != 0 || st.Tracks != 1 {
		t.Fatalf("second pass: %+v", st)
	}
}

func TestPrefetchLeavesAFileAViewerIsReadingForALaterPass(t *testing.T) {
	data := assFile(5)
	s := serve(t, data)
	viewing := true
	m := NewManager(Config{Dir: t.TempDir()})
	p := NewPrefetcher(m, func(ctx context.Context, path string) (*Target, error) {
		return &Target{
			File: File{Key: "viewed|1", Open: func(ctx context.Context) (Source, func(), error) {
				src, err := NewLinkSource(linkTo(s), int64(len(data)))
				return src, func() {}, err
			}},
			Streaming: func() bool { return viewing },
		}, nil
	})
	p.busyWait = time.Millisecond
	var st PrefetchState
	p.Run(context.Background(), []string{"/a.mkv"}, func(s PrefetchState) { st = s })
	if reqs, _, _ := s.stats(); reqs != 0 {
		t.Fatalf("%d request(s) for a file a viewer is reading", reqs)
	}
	if st.Skipped != 1 || st.Failed != 0 || st.Tracks != 0 {
		t.Fatalf("first pass: %+v", st)
	}
	viewing = false
	p.Run(context.Background(), []string{"/a.mkv"}, func(s PrefetchState) { st = s })
	if st.Skipped != 0 || st.Tracks != 1 {
		t.Fatalf("second pass: %+v", st)
	}
}

// A restart: a new Manager and Prefetcher on the same cache directory. The
// files the earlier run finished (the one with a gap included) cost no read and
// no question to the provider; a file whose cached track was evicted, or that
// changed, is read again.
func TestPrefetchDoneFilesSurviveARestart(t *testing.T) {
	film := makeFilm(t)
	type entry struct {
		data []byte
		s    *served
	}
	lib := map[string]*entry{}
	for name, data := range map[string][]byte{
		"/film.mkv":   film.data,
		"/plain.mkv":  assFile(10),
		"/broken.mkv": assFile(10, 4),
	} {
		lib[name] = &entry{data: data, s: serve(t, data)}
	}
	revision := map[string]string{}
	covers := 0
	open := func(ctx context.Context, path string) (*Target, error) {
		e := lib[path]
		tg := &Target{File: File{
			Key: "lib|" + path + "|" + revision[path],
			Open: func(ctx context.Context) (Source, func(), error) {
				src, err := NewLinkSource(linkTo(e.s), int64(len(e.data)))
				return src, func() {}, err
			},
		}}
		if path == "/film.mkv" {
			tg.Cover = func(context.Context) (func(TrackInfo) bool, error) {
				covers++
				return func(ti TrackInfo) bool { return ti.Order == 0 }, nil
			}
		}
		return tg, nil
	}
	requests := func() (n int) {
		for _, e := range lib {
			r, _, _ := e.s.stats()
			n += r
		}
		return n
	}
	dir := t.TempDir()
	paths := []string{"/film.mkv", "/plain.mkv", "/broken.mkv"}
	pass := func() (PrefetchState, *Manager) {
		m := NewManager(Config{Dir: dir})
		p := NewPrefetcher(m, open)
		p.busyWait = time.Millisecond
		var st PrefetchState
		p.Run(context.Background(), paths, func(s PrefetchState) { st = s })
		return st, m
	}

	if st, _ := pass(); st.Tracks != 6 || st.Failed != 0 {
		t.Fatalf("first run: %+v", st)
	}
	before, coversBefore := requests(), covers
	st, m := pass()
	if after := requests(); after != before {
		t.Errorf("the run after a restart made %d range request(s)", after-before)
	}
	if covers != coversBefore {
		t.Error("the provider was asked about a file that was done")
	}
	if st.Files != 3 || st.Tracks != 0 || st.Failed != 0 {
		t.Errorf("after the restart: %+v", st)
	}
	// the track list is known without reading too: a viewer's listing is free
	if tracks, err := m.Tracks(context.Background(), File{Key: "lib|/plain.mkv|"}); err != nil || len(tracks) != 1 {
		t.Errorf("tracks after the restart: %v %v", tracks, err)
	}

	// an evicted track: that file is not done any more
	plain := File{Key: "lib|/plain.mkv|"}
	for _, ext := range cacheExts {
		_ = os.Remove(m.disk.name(plain.Key, 0) + "." + ext)
	}
	if st, _ = pass(); st.Tracks != 1 {
		t.Errorf("after an eviction: %+v, want the one track read again", st)
	}
	// a changed file is another file
	revision["/plain.mkv"] = "2"
	if st, _ = pass(); st.Tracks != 1 {
		t.Errorf("after a change: %+v", st)
	}
}
