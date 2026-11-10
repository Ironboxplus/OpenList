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

// The storage allows one download connection per file: extraction reads through
// the gate as background reads, and a playback of the same file (a proxy stream
// OpenList is serving) comes first.

const gateKey = "/film.mkv"

func gatedFilm(t *testing.T, grace time.Duration, delay time.Duration) (f film, s *served, gate *stream.FileGate, src Source) {
	t.Helper()
	f = makeFilm(t)
	s = serve(t, f.data)
	s.delay = delay
	gate = &stream.FileGate{Grace: grace}
	src = &stream.GatedSource{Inner: s.source(t), Key: gateKey, Gate: gate}
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

func TestWhileAFileIsPlayedExtractionAsksTheOriginForNothing(t *testing.T) {
	_, s, gate, src := gatedFilm(t, 50*time.Millisecond, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	end := gate.BeginPlayback(gateKey)
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

func TestAStartingPlaybackCancelsTheReadInFlightAndExtractionResumesIdentically(t *testing.T) {
	f, s, gate, src := gatedFilm(t, 100*time.Millisecond, 40*time.Millisecond)
	old := groupItems
	groupItems = 1 // many small reads, so that one is surely in flight
	t.Cleanup(func() { groupItems = old })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	done := extractInBackground(ctx, src, 0)
	// a few blocks in, with a read in flight
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

	// a viewer starts playing the file: the origin serves that stream too
	started := time.Now()
	end := gate.BeginPlayback(gateKey)
	if took := time.Since(started); took > time.Second {
		t.Errorf("starting playback took %v to get the extraction's read out of the way", took)
	}
	// the cancelled read is gone from the origin within a moment
	for i := 0; s.inFlightNow() != 0; i++ {
		if i > 500 {
			t.Fatalf("%d origin request(s) still in flight half a second after playback began", s.inFlightNow())
		}
		time.Sleep(time.Millisecond)
	}
	req, _ := http.NewRequest("GET", s.ts.URL, nil)
	req.Header.Set("Range", "bytes=0-1023")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// no further read of the extraction reaches the origin while it plays
	before, _, _ := s.stats()
	time.Sleep(300 * time.Millisecond)
	if after, _, _ := s.stats(); after != before {
		t.Errorf("extraction read %d time(s) during playback", after-before)
	}
	end()

	var got extractResult
	select {
	case got = <-done:
	case <-time.After(time.Minute):
		t.Fatal("extraction never completed after playback")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if _, _, maxIn := s.stats(); maxIn != 1 {
		t.Errorf("the origin saw %d connections to the file at once", maxIn)
	}

	// the same bytes an uninterrupted run gives, and ffmpeg's cues
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

func TestPlaybacksNeverWaitForOneAnotherOrForBackground(t *testing.T) {
	g := &stream.FileGate{Grace: time.Hour}
	start := time.Now()
	ends := []func(){g.BeginPlayback("/a"), g.BeginPlayback("/a"), g.BeginPlayback("/a")}
	if time.Since(start) > 200*time.Millisecond {
		t.Errorf("three playbacks took %v to begin", time.Since(start))
	}
	for _, e := range ends {
		e()
	}
	// idle, but the grace has not passed: background still waits
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	ran := false
	err := g.RunBackground(ctx, "/a", func(context.Context) error { ran = true; return nil })
	if err == nil || ran {
		t.Errorf("background ran inside the grace: err=%v ran=%v", err, ran)
	}
	// another file is not held by it
	if err := g.RunBackground(context.Background(), "/b", func(context.Context) error { return nil }); err != nil {
		t.Errorf("another file's background read: %v", err)
	}
}
