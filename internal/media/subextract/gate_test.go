package subextract

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/stream"
)

// The storage allows N download connections per file: extraction reads through
// the gate as background reads, and a playback of the same file (a proxy stream
// OpenList is serving) takes slots first.

const gateKey = "/film.mkv"

func gatedFilm(t *testing.T, limit int, grace, delay time.Duration) (f film, s *served, gate *stream.FileGate, src Source) {
	t.Helper()
	f = makeFilm(t)
	s = serve(t, f.data)
	s.delay = delay
	gate = &stream.FileGate{Grace: grace}
	src = &stream.GatedSource{Inner: s.source(t), Key: gateKey, Gate: gate, Limit: func() int { return limit }}
	return
}

// extractTrack reads one track to the end, as the extractor does, without
// failing the test from another goroutine.
func extractTrack(ctx context.Context, src Source, order int) ([]byte, error) {
	task, err := newTask(ctx, src, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := task.Want(order, true, 0); err != nil {
		return nil, err
	}
	for {
		data, _, _, complete, err := task.Snapshot(order)
		if err != nil {
			return nil, err
		}
		if complete {
			return data, nil
		}
		wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		task.Wait(wctx, order)
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

type extractResult struct {
	data []byte
	err  error
}

func extractInBackground(ctx context.Context, src Source, order int) <-chan extractResult {
	out := make(chan extractResult, 1)
	go func() {
		d, err := extractTrack(ctx, src, order)
		out <- extractResult{d, err}
	}()
	return out
}

// stream is a proxy stream of the file as the origin sees it: a request that
// stays open until released.
func openStream(t *testing.T, s *served) (release func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", s.ts.URL, nil)
	req.Header.Set("Range", "bytes=0-1023")
	req.Header.Set("X-Hold", "1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func waitInFlight(t *testing.T, s *served, n int) {
	t.Helper()
	for i := 0; s.inFlightNow() != n; i++ {
		if i > 5000 {
			t.Fatalf("%d origin requests in flight, waited for %d", s.inFlightNow(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWithOneSlotAPlayedFileIsNotReadByExtraction(t *testing.T) {
	_, s, gate, src := gatedFilm(t, 1, 50*time.Millisecond, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	end := gate.BeginPlayback(gateKey, 1)
	done := extractInBackground(ctx, src, 0)
	time.Sleep(400 * time.Millisecond)
	if reqs, _, _ := s.stats(); reqs != 0 {
		t.Fatalf("%d request(s) reached the origin while the file was being played", reqs)
	}
	select {
	case r := <-done:
		t.Fatalf("extraction finished during playback: %v", r.err)
	default:
	}

	// playback over: after the grace the extraction goes on and completes
	end()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if len(parseCues(r.data)) != filmCues {
			t.Errorf("%d cues after the wait, want %d", len(parseCues(r.data)), filmCues)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("extraction never resumed")
	}
}

// interrupted runs an extraction with limit slots, lets playbacks take them one
// by one (each a held request at the origin) and checks that the origin never saw
// more than limit connections to the file and that the finished track is the one
// an uninterrupted run writes.
func interrupted(t *testing.T, limit int) {
	f, s, gate, src := gatedFilm(t, limit, 100*time.Millisecond, 40*time.Millisecond)
	old := groupItems
	groupItems = 1 // many small reads, so that one is surely in flight
	t.Cleanup(func() { groupItems = old })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	done := extractInBackground(ctx, src, 0)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if reqs, _, _ := s.stats(); reqs >= 8 && s.inFlightNow() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("extraction never got going")
		}
		time.Sleep(time.Millisecond)
	}

	var releases []func()
	var ends []func()
	for i := 0; i < limit; i++ {
		started := time.Now()
		end := gate.BeginPlayback(gateKey, limit)
		if took := time.Since(started); took > time.Second {
			t.Errorf("playback %d took %v to begin", i+1, took)
		}
		ends = append(ends, end)
		if i < limit-1 {
			// a slot is still free for extraction: it goes on beside the stream
			releases = append(releases, openStream(t, s))
			waitInFlight(t, s, i+2)
			before, _, _ := s.stats()
			time.Sleep(200 * time.Millisecond)
			if after, _, _ := s.stats(); after == before {
				t.Errorf("with %d of %d slots taken by playback extraction stood still", i+1, limit)
			}
			continue
		}
		// the last slot: extraction's read in flight is cancelled for it, and the
		// stream is the only connection to the file
		for j := 0; s.inFlightNow() > i; j++ {
			if j > 500 {
				t.Fatalf("%d origin requests in flight half a second after the last slot was taken", s.inFlightNow())
			}
			time.Sleep(time.Millisecond)
		}
		releases = append(releases, openStream(t, s))
		waitInFlight(t, s, limit)
	}
	before, _, _ := s.stats()
	time.Sleep(300 * time.Millisecond)
	if after, _, _ := s.stats(); after != before {
		t.Errorf("extraction read %d time(s) with every slot held by playback", after-before)
	}
	for _, r := range releases {
		r()
	}
	for _, e := range ends {
		e()
	}

	var got extractResult
	select {
	case got = <-done:
	case <-time.After(time.Minute):
		t.Fatal("extraction never completed after playback")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if _, _, maxIn := s.stats(); maxIn > limit {
		t.Errorf("the origin saw %d connections to the file at once, the limit is %d", maxIn, limit)
	}

	plain := serve(t, f.data)
	want, err := extractTrack(ctx, plain.source(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.data, want) {
		t.Errorf("the interrupted run wrote %d bytes, an uninterrupted one %d", len(got.data), len(want))
	}
	ref := reference(t, f.path, 0, []string{"-c:s", "copy"}, "srt")
	sameCues(t, "interrupted extraction", parseCues(got.data), parseCues(ref))
}

func TestWithOneSlotAStartingPlaybackCancelsTheReadInFlightAndExtractionResumesIdentically(t *testing.T) {
	interrupted(t, 1)
}

func TestWithTwoSlotsExtractionRunsBesideOneStreamAndGivesWayToTheSecond(t *testing.T) {
	interrupted(t, 2)
}

func TestPlaybacksNeverWaitForOneAnotherOrForBackground(t *testing.T) {
	g := &stream.FileGate{Grace: time.Hour}
	start := time.Now()
	// more playbacks than slots: none of them waits
	ends := []func(){g.BeginPlayback("/a", 2), g.BeginPlayback("/a", 2), g.BeginPlayback("/a", 2), g.BeginPlayback("/a", 2)}
	if time.Since(start) > 200*time.Millisecond {
		t.Errorf("four playbacks on two slots took %v to begin", time.Since(start))
	}
	for _, e := range ends {
		e()
	}
	// idle, but the grace has not passed: background still waits
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	ran := false
	err := g.RunBackground(ctx, "/a", func() int { return 2 }, func(context.Context) error { ran = true; return nil })
	if err == nil || ran {
		t.Errorf("background ran inside the grace: err=%v ran=%v", err, ran)
	}
	// another file is not held by it
	if err := g.RunBackground(context.Background(), "/b", func() int { return 2 }, func(context.Context) error { return nil }); err != nil {
		t.Errorf("another file's background read: %v", err)
	}
	// no limit: no gate, even on a file being played
	end := g.BeginPlayback("/c", 0)
	defer end()
	if err := g.RunBackground(context.Background(), "/c", func() int { return 0 }, func(context.Context) error { return nil }); err != nil {
		t.Errorf("a storage with no limit was held: %v", err)
	}
}
