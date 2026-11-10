package subextract

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// These tests make real files with ffmpeg and mkvmerge, serve their bytes over
// HTTP and read them back through the same range reader every driver link gets.
// CI has the tools (apt: ffmpeg, mkvtoolnix); elsewhere the tests skip, except
// that SUBEXTRACT_REQUIRE=1 turns a missing tool into a failure.

func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("SUBEXTRACT_REQUIRE") == "1" {
				t.Fatalf("%s is required (SUBEXTRACT_REQUIRE=1) and is not installed", tool)
			}
			t.Skipf("%s is not installed", tool)
		}
	}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		// mkvmerge exits 1 for warnings; the file is still written
		if ee, ok := err.(*exec.ExitError); ok && name == "mkvmerge" && ee.ExitCode() == 1 {
			return
		}
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out.String())
	}
}

// served is a file behind an HTTP server that honours Range and counts what it
// is asked: how many requests, how many bytes, and how many were ever in flight
// at once.
type served struct {
	ts       *httptest.Server
	data     []byte
	mu       sync.Mutex
	inflight int
	maxIn    int
	requests int
	bytes    int64
	offsets  []int64
	fail     func(n int) bool // makes the nth request (1-based) answer 403
}

var rangeRe = regexp.MustCompile(`^bytes=(\d+)-(\d*)$`)

func serve(t *testing.T, data []byte) *served {
	t.Helper()
	s := &served{data: data}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.inflight++
		if s.inflight > s.maxIn {
			s.maxIn = s.inflight
		}
		s.requests++
		n := s.requests
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.inflight--
			s.mu.Unlock()
		}()
		// long enough that two overlapping requests would be seen as such
		time.Sleep(2 * time.Millisecond)
		if s.fail != nil && s.fail(n) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		start, end := int64(0), int64(len(data))-1
		if m := rangeRe.FindStringSubmatch(r.Header.Get("Range")); m != nil {
			start, _ = strconv.ParseInt(m[1], 10, 64)
			if m[2] != "" {
				end, _ = strconv.ParseInt(m[2], 10, 64)
			}
			if end >= int64(len(data)) {
				end = int64(len(data)) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
		}
		s.mu.Lock()
		s.bytes += end - start + 1
		s.offsets = append(s.offsets, start)
		s.mu.Unlock()
		_, _ = w.Write(data[start : end+1])
	}))
	t.Cleanup(s.ts.Close)
	return s
}

func (s *served) source(t *testing.T) Source {
	t.Helper()
	src, err := NewLinkSource(&model.Link{URL: s.ts.URL}, int64(len(s.data)))
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func (s *served) stats() (requests int, bytes int64, maxInflight int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.bytes, s.maxIn
}

// extractAll reads the given tracks of the file to the end and returns each one
// as the file the extractor writes.
func extractAll(t *testing.T, src Source, orders ...int) map[int][]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	task, err := newTask(ctx, src, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orders {
		if err := task.Want(o, true, 0); err != nil {
			t.Fatalf("track %d: %v", o, err)
		}
	}
	out := map[int][]byte{}
	for _, o := range orders {
		for {
			data, _, _, complete, err := task.Snapshot(o)
			if err != nil {
				t.Fatalf("track %d: %v", o, err)
			}
			if complete {
				out[o] = data
				break
			}
			wctx, c := context.WithTimeout(ctx, 100*time.Millisecond)
			task.Wait(wctx, o)
			c()
			if ctx.Err() != nil {
				t.Fatalf("track %d: timed out", o)
			}
		}
	}
	return out
}

// reference is what ffmpeg writes for subtitle stream n of the file.
func reference(t *testing.T, file string, n int, codecArgs []string, format string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "ref."+format)
	args := []string{"-v", "error", "-y", "-i", file, "-map", fmt.Sprintf("0:s:%d", n)}
	args = append(args, codecArgs...)
	args = append(args, "-f", format, out)
	run(t, "ffmpeg", args...)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type srtCue struct {
	start, end int64
	text       string
}

var srtTimeRe = regexp.MustCompile(`(\d+):(\d\d):(\d\d)[,.](\d{3})\s*-->\s*(\d+):(\d\d):(\d\d)[,.](\d{3})`)

func clockMs(h, m, s, ms string) int64 {
	a, _ := strconv.Atoi(h)
	b, _ := strconv.Atoi(m)
	c, _ := strconv.Atoi(s)
	d, _ := strconv.Atoi(ms)
	return int64(((a*60+b)*60+c)*1000 + d)
}

// parseCues reads SRT or WebVTT text into cues, ignoring numbering, the VTT
// header and cue settings.
func parseCues(b []byte) []srtCue {
	var out []srtCue
	blocks := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n\n")
	for _, blk := range blocks {
		lines := strings.Split(strings.TrimSpace(blk), "\n")
		for i, l := range lines {
			m := srtTimeRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			text := strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
			if text == "" {
				break
			}
			out = append(out, srtCue{clockMs(m[1], m[2], m[3], m[4]), clockMs(m[5], m[6], m[7], m[8]), text})
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].text < out[j].text
	})
	return out
}

func dialogues(b []byte) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(l, "Dialogue:") {
			out = append(out, l)
		}
	}
	return out
}

func sameCues(t *testing.T, what string, got, want []srtCue) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: the reference has no cues", what)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: %d cues, ffmpeg wrote %d\nfirst of ours: %v\nfirst of theirs: %v", what, len(got), len(want), head(got), head(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: cue %d is %+v, ffmpeg wrote %+v", what, i, got[i], want[i])
		}
	}
}

func head(c []srtCue) []srtCue {
	if len(c) > 3 {
		return c[:3]
	}
	return c
}

// ── sources ────────────────────────────────────────────────────────────────

// srtText makes n cues, one second apart and 800 ms long, some with two lines,
// all with non-ASCII text.
func srtText(prefix string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		start := int64(i)*1000 + 200
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s %d 한국어 字幕\n", i+1, clock(start, ','), clock(start+800, ','), prefix, i)
		if i%3 == 0 {
			fmt.Fprintf(&b, "second line %d, with a comma\n", i)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func assText(n int) string {
	var b strings.Builder
	b.WriteString(`[Script Info]
ScriptType: v4.00+
PlayResX: 160
PlayResY: 90

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,10,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,1,0,2,5,5,5,1
Style: Sign,Arial,12,&H0000FFFF,&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,1,0,8,5,5,5,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
`)
	for i := 0; i < n; i++ {
		start := int64(i)*1000 + 100
		style := "Default"
		if i%4 == 0 {
			style = "Sign"
		}
		fmt.Fprintf(&b, "Dialogue: %d,%s,%s,%s,,0,0,0,,Hello, {\\i1}world{\\i0} %d\\Nsecond\n", i%2, assTimeText(start), assTimeText(start+900), style, i)
	}
	return b.String()
}

func assTimeText(ms int64) string { return assClock(ms / 10) }

// sup makes a PGS stream: n display sets four seconds apart, each cleared two
// seconds later. The bitmaps are valid in structure only, which is all a muxer
// or a copy looks at.
func sup(n int) []byte {
	var b bytes.Buffer
	seg := func(pts uint32, typ byte, payload []byte) {
		b.WriteString("PG")
		_ = binary.Write(&b, binary.BigEndian, pts)
		_ = binary.Write(&b, binary.BigEndian, uint32(0))
		b.WriteByte(typ)
		_ = binary.Write(&b, binary.BigEndian, uint16(len(payload)))
		b.Write(payload)
	}
	be16 := func(v int) []byte { return []byte{byte(v >> 8), byte(v)} }
	for i := 0; i < n; i++ {
		pts := uint32((i*4000 + 500) * 90)
		pcs := append(be16(160), be16(90)...)
		pcs = append(pcs, 0x10)
		pcs = append(pcs, be16(i*2)...)
		pcs = append(pcs, 0x80, 0, 0, 1)
		pcs = append(pcs, be16(0)...)
		pcs = append(pcs, 0, 0)
		pcs = append(pcs, be16(10)...)
		pcs = append(pcs, be16(60)...)
		seg(pts, 0x16, pcs)
		wds := []byte{1, 0}
		wds = append(wds, be16(10)...)
		wds = append(wds, be16(60)...)
		wds = append(wds, be16(4)...)
		wds = append(wds, be16(2)...)
		seg(pts, 0x17, wds)
		seg(pts, 0x14, []byte{0, 0, 1, 235, 128, 128, 255})
		ods := append(be16(0), 0, 0xC0)
		rle := []byte{1, 1, 1, 1, 0, 0, 1, 1, 1, 1, 0, 0}
		ods = append(ods, byte((len(rle)+4)>>16), byte((len(rle)+4)>>8), byte(len(rle)+4))
		ods = append(ods, be16(4)...)
		ods = append(ods, be16(2)...)
		ods = append(ods, rle...)
		seg(pts, 0x15, ods)
		seg(pts, 0x80, nil)

		clearAt := uint32((i*4000 + 2500) * 90)
		pcs = append(be16(160), be16(90)...)
		pcs = append(pcs, 0x10)
		pcs = append(pcs, be16(i*2+1)...)
		pcs = append(pcs, 0x00, 0, 0, 0)
		seg(clearAt, 0x16, pcs)
		seg(clearAt, 0x80, nil)
	}
	return b.Bytes()
}

// noiseVideo is a video whose frames do not compress, so that the bytes between
// two subtitle blocks are many, as they are in a real film.
func noiseVideo(t *testing.T, dir string, seconds int, ext string) string {
	t.Helper()
	out := filepath.Join(dir, "base."+ext)
	run(t, "ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("nullsrc=s=320x180:r=12:d=%d,geq=random(1)*255:128:128", seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "20", "-g", "24", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "48k", out)
	return out
}

func linkTo(s *served) *model.Link { return &model.Link{URL: s.ts.URL} }
