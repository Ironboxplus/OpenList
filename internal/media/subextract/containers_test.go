package subextract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A subtitle-only file made by hand, with the cases real muxers do not give on
// request: every kind of lacing, header stripping, Clusters whose size field is
// not the length the first one's was, SimpleBlocks. ffmpeg reads it too, and
// what it makes of it is the reference.
func TestHandBuiltMatroska(t *testing.T) {
	needTools(t, "ffmpeg")
	var blocks []bBlock
	text := func(s string) []byte { return []byte(s) }
	// 0: plain blocks, some in Clusters with a wide size field
	for i := 0; i < 12; i++ {
		blocks = append(blocks, bBlock{track: 1, ms: int64(i)*1000 + 100, dur: 700, frames: [][]byte{text(fmt.Sprintf("plain %d", i))}, wideCluster: i%5 == 3})
	}
	// 1: laced, one kind per block
	frames := func(prefix string, n int) [][]byte {
		var fs [][]byte
		for i := 0; i < n; i++ {
			fs = append(fs, text(fmt.Sprintf("%s %d %s", prefix, i, strings.Repeat("x", i*37%300))))
		}
		return fs
	}
	blocks = append(blocks,
		bBlock{track: 2, ms: 100, dur: 3000, lacing: 1, frames: frames("xiph", 3)},
		bBlock{track: 2, ms: 4100, dur: 3000, lacing: 3, frames: frames("ebml", 4)},
		bBlock{track: 2, ms: 8100, dur: 3000, lacing: 2, frames: [][]byte{text("fixed AAAA"), text("fixed BBBB"), text("fixed CCCC")}},
		bBlock{track: 2, ms: 12100, dur: 900, frames: frames("single", 1)},
	)
	// 2: header stripping, written as SimpleBlocks (no duration of their own)
	for i := 0; i < 5; i++ {
		blocks = append(blocks, bBlock{track: 3, ms: int64(i)*2000 + 300, dur: 0, simple: true, frames: [][]byte{text(fmt.Sprintf("tail %d", i))}})
	}
	// blocks must be in time order for the clusters to be
	sortBlocks(blocks)
	tracks := []bTrack{
		{num: 1, codec: "S_TEXT/UTF8", lang: "eng", name: "plain"},
		{num: 2, codec: "S_TEXT/UTF8", lang: "fra", name: "laced"},
		{num: 3, codec: "S_TEXT/UTF8", lang: "deu", stripped: []byte("Header ")},
	}
	data := buildMKV(tracks, blocks, 4000, true)
	path := writeFile(t, "built.mkv", data)
	s := serve(t, data)

	got := extractAll(t, s.source(t), 0, 1, 2)
	for n, name := range []string{"plain", "laced", "stripped"} {
		ref := reference(t, path, n, []string{"-c:s", "copy"}, "srt")
		want := parseCues(ref)
		if n == 2 {
			// ffmpeg puts the stripped header back as well
			for i := range want {
				if !strings.HasPrefix(want[i].text, "Header ") {
					t.Fatalf("ffmpeg's cue %d of the stripped track is %q", i, want[i].text)
				}
			}
		}
		sameCues(t, name, parseCues(got[n]), want)
	}
	if _, _, maxIn := s.stats(); maxIn != 1 {
		t.Errorf("%d requests in flight at once", maxIn)
	}
}

func sortBlocks(b []bBlock) {
	for i := 1; i < len(b); i++ {
		for j := i; j > 0 && b[j].ms < b[j-1].ms; j-- {
			b[j], b[j-1] = b[j-1], b[j]
		}
	}
}

func TestMatroskaWithoutCues(t *testing.T) {
	blocks := []bBlock{{track: 1, ms: 100, dur: 500, frames: [][]byte{[]byte("x")}}}
	tracks := []bTrack{{num: 1, codec: "S_TEXT/UTF8", lang: "eng"}}
	data := buildMKV(tracks, blocks, 4000, false)
	s := serve(t, data)
	ts, err := Probe(context.Background(), s.source(t))
	if err != nil || len(ts) != 1 || !ts[0].Supported {
		t.Fatalf("probe: %v %+v", err, ts)
	}
	task, err := newTask(context.Background(), s.source(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Want(0, true, 0); err != ErrNoCues {
		t.Fatalf("Want = %v, want ErrNoCues", err)
	}
}

func TestUnsupportedCodecsAreListedNotExtracted(t *testing.T) {
	tracks := []bTrack{{num: 1, codec: "S_VOBSUB", lang: "eng"}, {num: 2, codec: "S_TEXT/UTF8", lang: "eng"}}
	blocks := []bBlock{{track: 2, ms: 100, dur: 500, frames: [][]byte{[]byte("x")}}}
	data := buildMKV(tracks, blocks, 4000, true)
	s := serve(t, data)
	ts, err := Probe(context.Background(), s.source(t))
	if err != nil || len(ts) != 2 || ts[0].Supported || ts[0].Format != "" || !ts[1].Supported {
		t.Fatalf("probe: %v %+v", err, ts)
	}
}

// toVTT is an SRT file as WebVTT: the header, and a full stop for the comma in
// the timestamps.
func toVTT(srt []byte) []byte {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, l := range strings.Split(strings.ReplaceAll(string(srt), "\r\n", "\n"), "\n") {
		if strings.Contains(l, "-->") {
			l = strings.ReplaceAll(l, ",", ".")
		}
		b.WriteString(l + "\n")
	}
	return []byte(b.String())
}

func TestMP4TracksMatchFFmpeg(t *testing.T) {
	needTools(t, "ffmpeg")
	dir := t.TempDir()
	base := noiseVideo(t, dir, 30, "mp4")
	en := filepath.Join(dir, "en.srt")
	zh := filepath.Join(dir, "zh.srt")
	if err := os.WriteFile(en, []byte(srtText("English", 25)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zh, []byte(srtText("中文", 25)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, codec, format string
		refArgs             []string
		refFormat           string
		sample              string
	}{
		{"mov_text", "mov_text", FormatSRT, []string{"-c:s", "srt"}, "srt", "tx3g"},
		{"webvtt", "webvtt", FormatVTT, []string{"-c:s", "webvtt"}, "webvtt", "wvtt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, tc.name+".mp4")
			if tc.codec == "webvtt" {
				// ffmpeg's mp4 muxer has no tag for WebVTT; GPAC's MP4Box writes wvtt.
				needTools(t, "MP4Box")
				for _, n := range []string{"en", "zh"} {
					srt, err := os.ReadFile(filepath.Join(dir, n+".srt"))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, n+".vtt"), toVTT(srt), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				_ = os.Remove(out)
				run(t, "MP4Box", "-add", base,
					"-add", filepath.Join(dir, "en.vtt")+":lang=eng", "-add", filepath.Join(dir, "zh.vtt")+":lang=chi", out)
			} else {
				run(t, "ffmpeg", "-v", "error", "-y", "-i", base, "-i", en, "-i", zh,
					"-map", "0:v", "-map", "0:a", "-map", "1", "-map", "2",
					"-c:v", "copy", "-c:a", "copy", "-c:s", tc.codec,
					"-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=chi",
					"-movflags", "+faststart", out)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			s := serve(t, data)
			tracks, err := Probe(context.Background(), s.source(t))
			if err != nil || len(tracks) != 2 {
				t.Fatalf("probe: %v %+v", err, tracks)
			}
			for i, want := range []string{"eng", "chi"} {
				if tracks[i].Language != want || tracks[i].Format != tc.format || tracks[i].Codec != tc.sample || !tracks[i].Supported {
					t.Errorf("track %d: %+v", i, tracks[i])
				}
			}
			got := extractAll(t, s.source(t), 0, 1)
			for i, n := range []string{"en", "zh"} {
				var ref []byte
				if tc.codec == "webvtt" {
					// FFmpeg's mov demuxer does not list a wvtt track as a stream, so
					// the reference is the WebVTT file MP4Box was given.
					if ref, err = os.ReadFile(filepath.Join(dir, n+".vtt")); err != nil {
						t.Fatal(err)
					}
				} else {
					ref = reference(t, out, i, tc.refArgs, tc.refFormat)
				}
				sameCues(t, fmt.Sprintf("%s track %d", tc.name, i), parseCues(got[i]), parseCues(ref))
			}
			if _, nbytes, maxIn := s.stats(); maxIn != 1 || nbytes > int64(len(data))/4 {
				t.Errorf("max in flight %d, read %d of %d bytes", maxIn, nbytes, len(data))
			}
		})
	}
}
