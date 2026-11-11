package subextract

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func filmFile(s *served, f film) File {
	return File{
		Key: "fill|/film.mkv|1|1",
		Open: func(ctx context.Context) (Source, func(), error) {
			src, err := NewLinkSource(linkTo(s), int64(len(f.data)))
			return src, func() {}, err
		},
	}
}

func TestFillLeavesTheTracksOnDisk(t *testing.T) {
	f := makeFilm(t)
	s := serve(t, f.data)
	m := NewManager(Config{Dir: t.TempDir()})
	file := filmFile(s, f)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := m.Fill(ctx, file, []int{0, 3, 4}); err != nil {
		t.Fatal(err)
	}
	for _, o := range []int{0, 3, 4} {
		if !m.Cached(file, o) {
			t.Errorf("track %d is not on disk after Fill", o)
		}
	}
	if m.Cached(file, 1) {
		t.Error("track 1 was not asked for but is on disk")
	}
	got, _, _ := m.disk.Get(file.Key, 0)
	ref := reference(t, f.path, 0, []string{"-c:s", "copy"}, "srt")
	sameCues(t, "track 0 from Fill", parseCues(got), parseCues(ref))
	if _, _, maxIn := s.stats(); maxIn != 1 {
		t.Errorf("%d requests in flight at once", maxIn)
	}

	// Everything asked for is on disk: nothing is read again.
	before, _, _ := s.stats()
	if err := m.Fill(ctx, file, []int{0, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if after, _, _ := s.stats(); after != before {
		t.Errorf("a second Fill cost %d request(s)", after-before)
	}
}

func TestFillStopsReadingWhenItsContextEnds(t *testing.T) {
	f := makeFilm(t)
	old := groupItems
	groupItems = 1
	t.Cleanup(func() { groupItems = old })

	s := serve(t, f.data)
	m := NewManager(Config{Dir: t.TempDir()})
	file := filmFile(s, f)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := m.Fill(ctx, file, []int{0, 1, 2}); err == nil {
		t.Skip("the file was read to the end before the context ended")
	}
	time.Sleep(200 * time.Millisecond)
	a, _, _ := s.stats()
	time.Sleep(200 * time.Millisecond)
	if b, _, _ := s.stats(); b != a {
		t.Errorf("reading went on after Fill gave up: %d then %d requests", a, b)
	}
	m.mu.Lock()
	n := len(m.tasks)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("%d task(s) left behind", n)
	}
}

func TestDiskCacheDropsTheLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	c := &diskCache{dir: dir, max: func() int64 { return 250 }}
	data := make([]byte, 100)
	for i, key := range []string{"a", "b"} {
		if err := c.Put(key, 0, FormatSRT, data); err != nil {
			t.Fatal(err)
		}
		// distinct modification times, a before b
		old := time.Now().Add(time.Duration(i-10) * time.Minute)
		if err := os.Chtimes(c.name(key, 0)+"."+FormatSRT, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// reading a makes it the most recently used
	if _, _, ok := c.Get("a", 0); !ok {
		t.Fatal("a is not cached")
	}
	if err := c.Put("c", 0, FormatSRT, data); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.Get("b", 0); ok {
		t.Error("b, the least recently used, is still cached")
	}
	for _, k := range []string{"a", "c"} {
		if _, _, ok := c.Get(k, 0); !ok {
			t.Errorf("%s was dropped", k)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*")); len(m) != 2 {
		t.Errorf("%d files in the cache directory, want 2", len(m))
	}
}

// A probe slower than the 2 s the listing used to wait is still listed whole
// the first time.
func TestListingHasEveryTrackOfASlowProbeTheFirstTime(t *testing.T) {
	f := makeFilm(t)
	s := serve(t, f.data)
	s.delay = 2200 * time.Millisecond
	m := NewManager(Config{Dir: t.TempDir()})
	file := filmFile(s, f)

	tracks, err := m.TracksShared(context.Background(), file)
	if err != nil || len(tracks) != 5 {
		t.Fatalf("first listing: err=%v, %d tracks, want 5", err, len(tracks))
	}
}

func TestListingsAtOnceShareOneProbeAndReadNothingElse(t *testing.T) {
	f := makeFilm(t)
	s := serve(t, f.data)
	s.delay = 400 * time.Millisecond
	m := NewManager(Config{Dir: t.TempDir()})
	file := filmFile(s, f)

	const callers = 3
	got := make(chan int, callers)
	for i := 0; i < callers; i++ {
		go func() {
			tracks, err := m.TracksShared(context.Background(), file)
			if err != nil {
				t.Error(err)
			}
			got <- len(tracks)
		}()
	}
	for i := 0; i < callers; i++ {
		if n := <-got; n != 5 {
			t.Fatalf("%d tracks", n)
		}
	}
	// once: one probe for all the asking, and no block of any track read after it
	n, _, maxIn := s.stats()
	time.Sleep(300 * time.Millisecond)
	if after, _, _ := s.stats(); after != n || maxIn != 1 {
		t.Errorf("requests went from %d to %d, %d in flight at once: listing read more than the head", n, after, maxIn)
	}
	if n > 2 {
		t.Errorf("%d requests for %d listings", n, callers)
	}
	for o := 0; o < 5; o++ {
		if m.Cached(file, o) {
			t.Errorf("track %d is on disk after a listing", o)
		}
	}

	// known now: answered from memory, nothing read
	start := time.Now()
	if tracks, err := m.TracksShared(context.Background(), file); err != nil || len(tracks) != 5 {
		t.Fatalf("a probed file: err=%v, %d tracks", err, len(tracks))
	}
	if after, _, _ := s.stats(); after != n || time.Since(start) > 100*time.Millisecond {
		t.Error("a probed file is not answered from memory")
	}
}

func TestListingEndsWithItsRequestAndTheProbeCarriesOn(t *testing.T) {
	f := makeFilm(t)
	s := serve(t, f.data)
	s.delay = 400 * time.Millisecond
	m := NewManager(Config{Dir: t.TempDir()})
	file := filmFile(s, f)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if tracks, err := m.TracksShared(ctx, file); !errors.Is(err, context.DeadlineExceeded) || tracks != nil {
		t.Fatalf("a cancelled listing: %d tracks, err=%v", len(tracks), err)
	}

	// the next request joins the probe that is still running
	tracks, err := m.TracksShared(context.Background(), file)
	if err != nil || len(tracks) != 5 {
		t.Fatalf("listing after the cancelled one: err=%v, %d tracks", err, len(tracks))
	}
	if n, _, _ := s.stats(); n > 2 {
		t.Errorf("%d requests: the probe was started again", n)
	}
}
