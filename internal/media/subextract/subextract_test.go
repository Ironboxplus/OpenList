package subextract

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	fixtureOnce sync.Once
	fixtureDir  string
	fixtureErr  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fixtureDir != "" {
		_ = os.RemoveAll(fixtureDir)
	}
	os.Exit(code)
}

// film is a Matroska file made once for the tests that read it: noise video
// (so the bytes between subtitle blocks are many), and five subtitle tracks.
//
//	0 English  S_TEXT/UTF8   named, eng
//	1 简体中文   S_TEXT/UTF8   named, chi, zlib compressed
//	2 (none)   S_TEXT/UTF8   no name, kor
//	3 Styled   S_TEXT/ASS    jpn
//	4 (none)   S_HDMV/PGS    eng
type film struct {
	path string
	data []byte
	dir  string
}

const (
	filmSeconds = 62
	filmCues    = 60
	filmPGS     = 15
)

func makeFilm(t *testing.T) film {
	t.Helper()
	needTools(t, "ffmpeg", "mkvmerge")
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "subextract-")
		if err != nil {
			fixtureErr = err.Error()
			return
		}
		fixtureDir = dir
	})
	if fixtureErr != "" {
		t.Fatal(fixtureErr)
	}
	out := filepath.Join(fixtureDir, "film.mkv")
	if _, err := os.Stat(out); err != nil {
		base := noiseVideo(t, fixtureDir, filmSeconds, "mkv")
		write := func(name, text string) string {
			p := filepath.Join(fixtureDir, name)
			if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}
		en := write("en.srt", srtText("English", filmCues))
		zh := write("zh.srt", srtText("中文", filmCues))
		ko := write("ko.srt", srtText("Korean", filmCues))
		ass := write("styled.ass", assText(filmCues))
		pgs := write("pgs.sup", string(sup(filmPGS)))
		run(t, "mkvmerge", "-o", out, base,
			"--language", "0:eng", "--track-name", "0:English", en,
			"--language", "0:chi", "--track-name", "0:简体中文", "--compression", "0:zlib", zh,
			"--language", "0:kor", ko,
			"--language", "0:jpn", "--track-name", "0:Styled", ass,
			"--language", "0:eng", pgs)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return film{path: out, data: data, dir: fixtureDir}
}

func TestProbeListsTheTracks(t *testing.T) {
	f := makeFilm(t)
	s := serve(t, f.data)
	tracks, err := Probe(context.Background(), s.source(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []TrackInfo{
		{0, "S_TEXT/UTF8", "eng", "English", FormatSRT, true},
		{1, "S_TEXT/UTF8", "chi", "简体中文", FormatSRT, true},
		{2, "S_TEXT/UTF8", "kor", "", FormatSRT, true},
		{3, "S_TEXT/ASS", "jpn", "Styled", FormatASS, true},
		{4, "S_HDMV/PGS", "eng", "", FormatSUP, true},
	}
	if len(tracks) != len(want) {
		t.Fatalf("tracks: %+v", tracks)
	}
	for i := range want {
		if tracks[i] != want[i] {
			t.Errorf("track %d is %+v, want %+v", i, tracks[i], want[i])
		}
	}
	reqs, bytes, maxIn := s.stats()
	t.Logf("probe of a %d byte file: %d request(s), %d bytes", len(f.data), reqs, bytes)
	if maxIn != 1 {
		t.Errorf("%d requests in flight at once", maxIn)
	}
	if reqs > 3 || bytes > 200<<10 {
		t.Errorf("probing cost %d requests and %d bytes", reqs, bytes)
	}
}

func TestMatroskaTracksMatchFFmpeg(t *testing.T) {
	f := makeFilm(t)
	s := serve(t, f.data)
	got := extractAll(t, s.source(t), 0, 1, 2, 3, 4)

	for n, name := range map[int]string{0: "SRT", 1: "SRT in zlib", 2: "SRT, no name"} {
		ref := reference(t, f.path, n, []string{"-c:s", "copy"}, "srt")
		sameCues(t, name, parseCues(got[n]), parseCues(ref))
		if c := len(parseCues(got[n])); c != filmCues {
			t.Errorf("%s: %d cues, the source has %d", name, c, filmCues)
		}
	}

	ref := reference(t, f.path, 3, []string{"-c:s", "copy"}, "ass")
	gotD, refD := dialogues(got[3]), dialogues(ref)
	if len(refD) != filmCues || len(gotD) != len(refD) {
		t.Fatalf("ASS: %d events, ffmpeg wrote %d, source has %d", len(gotD), len(refD), filmCues)
	}
	for i := range refD {
		if gotD[i] != refD[i] {
			t.Fatalf("ASS event %d is %q, ffmpeg wrote %q", i, gotD[i], refD[i])
		}
	}
	for _, want := range []string{"[Script Info]", "Style: Sign,", "[Events]"} {
		if !strings.Contains(string(got[3]), want) {
			t.Errorf("ASS header lacks %q", want)
		}
	}

	if ref := reference(t, f.path, 4, []string{"-c:s", "copy"}, "sup"); !bytes.Equal(got[4], ref) {
		t.Errorf("SUP: %d bytes, ffmpeg wrote %d", len(got[4]), len(ref))
		for i := 0; i < len(ref) && i < len(got[4]); i++ {
			if ref[i] != got[4][i] {
				t.Errorf("first difference at byte %d: %02x, ffmpeg %02x", i, got[4][i], ref[i])
				break
			}
		}
	}

	// One connection, and a read bill far below the file: the video between
	// the subtitle blocks was not read.
	reqs, nbytes, maxIn := s.stats()
	t.Logf("%d byte file, five tracks: %d request(s), %d bytes", len(f.data), reqs, nbytes)
	if maxIn != 1 {
		t.Errorf("%d requests in flight at once", maxIn)
	}
	if nbytes > int64(len(f.data))/10 {
		t.Errorf("read %d of %d bytes", nbytes, len(f.data))
	}
	// One request per cue time at most (the tracks' blocks of one instant are
	// neighbours) plus the structure.
	if limit := filmCues + filmPGS*2 + 10; reqs > limit {
		t.Errorf("%d requests, more than %d", reqs, limit)
	}
}

func TestExtractionStartsAtThePlaybackPosition(t *testing.T) {
	f := makeFilm(t)
	old := groupItems
	groupItems = 6
	t.Cleanup(func() { groupItems = old })

	s := serve(t, f.data)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	task, err := newTask(ctx, s.source(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Want(0, true, 40_000); err != nil {
		t.Fatal(err)
	}
	task.Wait(ctx, 0)
	task.mu.Lock()
	first := task.tracks[0].cues[0]
	task.mu.Unlock()
	if first.Start < 40_000-pivotLookback {
		t.Fatalf("the first cue read starts at %d ms, before the position 40000 ms less the lookback", first.Start)
	}
	data, _, done, complete, err := task.Snapshot(0)
	if err != nil {
		t.Fatal(err)
	}
	if !complete && (done <= 0 || done >= 1) {
		t.Errorf("done = %v while partial", done)
	}
	if len(parseCues(data)) == 0 {
		t.Error("the partial file has no cues")
	}

	// The part before the position is read after it, and the whole is right.
	for {
		_, _, _, complete, err := task.Snapshot(0)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			break
		}
		wctx, c := context.WithTimeout(ctx, 50*time.Millisecond)
		task.Wait(wctx, 0)
		c()
		if ctx.Err() != nil {
			t.Fatal("never finished")
		}
	}
	data, _, _, _, _ = task.Snapshot(0)
	ref := reference(t, f.path, 0, []string{"-c:s", "copy"}, "srt")
	sameCues(t, "after wrapping round", parseCues(data), parseCues(ref))
}

func TestManagerCachesAndSharesOneTask(t *testing.T) {
	f := makeFilm(t)
	s := serve(t, f.data)
	m := NewManager(Config{Dir: t.TempDir()})
	file := File{
		Key: "m|/film.mkv|1|1",
		Open: func(ctx context.Context) (Source, func(), error) {
			src, err := NewLinkSource(linkTo(s), int64(len(f.data)))
			return src, func() {}, err
		},
	}
	ctx := context.Background()

	if _, err := m.Tracks(ctx, file); err != nil {
		t.Fatal(err)
	}
	r0, _, _ := s.stats()
	if _, err := m.Tracks(ctx, file); err != nil {
		t.Fatal(err)
	}
	if r1, _, _ := s.stats(); r1 != r0 {
		t.Errorf("the second Tracks cost %d request(s)", r1-r0)
	}

	// Several callers at once, for different tracks of the same file.
	var wg sync.WaitGroup
	results := make([]Result, 3)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				r, err := m.Get(ctx, file, i, 0, 500*time.Millisecond)
				if err != nil {
					t.Error(err)
					return
				}
				if !r.Partial {
					results[i] = r
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		ref := reference(t, f.path, i, []string{"-c:s", "copy"}, "srt")
		sameCues(t, fmt.Sprintf("track %d through the manager", i), parseCues(r.Data), parseCues(ref))
	}
	if _, _, maxIn := s.stats(); maxIn != 1 {
		t.Errorf("%d requests in flight at once for one file", maxIn)
	}

	// Finished tracks are on disk: asking again reads nothing.
	before, _, _ := s.stats()
	time.Sleep(200 * time.Millisecond)
	for i := range results {
		r, err := m.Get(ctx, file, i, 0, time.Second)
		if err != nil || r.Partial || !bytes.Equal(r.Data, results[i].Data) {
			t.Fatalf("track %d from the cache: err=%v partial=%v", i, err, r.Partial)
		}
	}
	if after, _, _ := s.stats(); after != before {
		t.Errorf("the cache hits cost %d request(s)", after-before)
	}
	if !m.Cached(file, 0) {
		t.Error("Cached says no")
	}
}

func TestRefusedReadsAreRetriedOnOneConnection(t *testing.T) {
	f := makeFilm(t)
	old := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = old })

	s := serve(t, f.data)
	// the 4th and 5th requests are refused, as a drive does a second connection
	s.fail = func(n int) bool { return n == 4 || n == 5 }
	got := extractAll(t, s.source(t), 0)
	ref := reference(t, f.path, 0, []string{"-c:s", "copy"}, "srt")
	sameCues(t, "after refused reads", parseCues(got[0]), parseCues(ref))
	if _, _, maxIn := s.stats(); maxIn != 1 {
		t.Errorf("%d requests in flight at once", maxIn)
	}
}

func TestUnsupportedContainer(t *testing.T) {
	s := serve(t, bytes.Repeat([]byte("not a video "), 1000))
	if _, err := Probe(context.Background(), s.source(t)); err != ErrUnsupportedContainer {
		t.Fatalf("got %v", err)
	}
}
