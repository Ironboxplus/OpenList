package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/syncjob"
	"github.com/spf13/cobra"
)

func TestEndpointArg(t *testing.T) {
	if got, _ := endpointArg("/115/backup"); got != "/115/backup" {
		t.Fatalf("OpenList path rewritten to %q", got)
	}
	got, err := endpointArg("data/photos")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs("data/photos")
	if got != syncjob.LocalPrefix+want {
		t.Fatalf("relative local dir = %q, want %q", got, syncjob.LocalPrefix+want)
	}
	absData, _ := filepath.Abs("data")
	if got, _ := endpointArg("local:data"); got != syncjob.LocalPrefix+absData {
		t.Fatalf("local:data = %q, want %q", got, syncjob.LocalPrefix+absData)
	}
}

// fakeServer answers like the real /api/admin/sync endpoints: the job is
// running for the first two polls, then ends in finalState.
func fakeServer(t *testing.T, finalState syncjob.State, finalErr string) (*httptest.Server, *syncjob.Spec) {
	srv, started, _, _ := fakeServerCounting(t, finalState, finalErr)
	return srv, started
}

// fakeServerCounting also reports how many times the job was polled and
// cancelled.
func fakeServerCounting(t *testing.T, finalState syncjob.State, finalErr string) (*httptest.Server, *syncjob.Spec, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var started syncjob.Spec
	var polls, cancels atomic.Int32
	reply := func(w http.ResponseWriter, code int, msg string, data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg, "data": data})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "tok" {
			reply(w, 401, "unauthorized", nil)
			return
		}
		switch r.URL.Path {
		case "/api/admin/sync/run":
			_ = json.NewDecoder(r.Body).Decode(&started)
			if started.Mode == "mirror" {
				reply(w, 400, `unknown mode "mirror"`, nil)
				return
			}
			reply(w, 200, "success", syncjob.Snapshot{ID: "j1", Spec: started, State: syncjob.StateRunning})
		case "/api/admin/sync/get":
			s := syncjob.Snapshot{ID: "j1", Spec: started, State: syncjob.StateRunning, Stats: map[string]any{"bytes": 2048, "transfers": 1}}
			if polls.Add(1) > 2 {
				s.State, s.Error = finalState, finalErr
			}
			reply(w, 200, "success", s)
		case "/api/admin/sync/cancel":
			cancels.Add(1)
			reply(w, 200, "success", nil)
		default:
			reply(w, 404, "not found", nil)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &started, &polls, &cancels
}

func TestClientStartsAndFollowsAJobToCompletion(t *testing.T) {
	srv, started := fakeServer(t, syncjob.StateSucceeded, "")
	c := &syncClient{base: srv.URL, token: "tok", http: srv.Client()}
	snap, err := c.start(syncjob.Spec{Mode: syncjob.ModeSync, Src: "local:/x", Dst: "/115/x", Filters: []string{"- *.tmp"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if snap.ID != "j1" || started.Dst != "/115/x" || len(started.Filters) != 1 {
		t.Fatalf("server saw %+v, client got %+v", started, snap)
	}
	if err := c.follow(context.Background(), "j1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
}

func TestClientReportsFailuresAndServerErrors(t *testing.T) {
	srv, _ := fakeServer(t, syncjob.StateFailed, "corrupted on transfer")
	c := &syncClient{base: srv.URL, token: "tok", http: srv.Client()}
	if _, err := c.start(syncjob.Spec{Mode: "mirror"}); err == nil || !strings.Contains(err.Error(), "mirror") {
		t.Fatalf("server-side rejection not surfaced: %v", err)
	}
	if err := c.follow(context.Background(), "j1"); err == nil || !strings.Contains(err.Error(), "corrupted on transfer") {
		t.Fatalf("failed job not reported as an error: %v", err)
	}
	bad := &syncClient{base: srv.URL, token: "wrong", http: srv.Client()}
	if _, err := bad.start(syncjob.Spec{Mode: syncjob.ModeCopy}); err == nil {
		t.Fatalf("wrong token accepted")
	}
}

func TestProgressLineReadsJSONNumbers(t *testing.T) {
	var s syncjob.Snapshot
	if err := json.Unmarshal([]byte(`{"state":"running","stats":{"bytes":1572864,"totalBytes":3145728,"transfers":2,"totalTransfers":5,"errors":1}}`), &s); err != nil {
		t.Fatal(err)
	}
	line := progressLine(s)
	for _, want := range []string{"1.5 MiB / 3.0 MiB", "2/5 files", "1 errors"} {
		if !strings.Contains(line, want) {
			t.Fatalf("progress line %q lacks %q", line, want)
		}
	}
}

func TestSummaryLineSeparatesRapidFromRealUploads(t *testing.T) {
	line := summaryLine(syncjob.Snapshot{State: syncjob.StateSucceeded, Counters: syncjob.Counters{
		Uploaded: 5, UploadedBytes: 3 << 20, RapidUploads: 2, RapidBytes: 10 << 30, Match: 7,
	}})
	for _, want := range []string{"3 uploaded (3.0 MiB)", "2 rapid uploads (10.0 GiB not transferred)", "7 unchanged"} {
		if !strings.Contains(line, want) {
			t.Fatalf("summary %q lacks %q", line, want)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"8M": 8 << 20, "8m": 8 << 20, "1G": 1 << 30, "512K": 512 << 10, "64MiB": 64 << 20, "2KB": 2 << 10,
		"1024": 1024, "1.5M": 3 << 19, " 8M ": 8 << 20, "1T": 1 << 40,
	} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "M", "eight", "-1M", "8X"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) accepted", bad)
		}
	}
	d := syncjob.DefaultPackOptions()
	if formatSize(d.Threshold) != "8M" || formatSize(d.MaxPackBytes) != "1G" || formatSize(1000) != "1000" {
		t.Errorf("formatSize defaults = %s %s %s", formatSize(d.Threshold), formatSize(d.MaxPackBytes), formatSize(1000))
	}
}

func TestPackFlagsBuildTheSpec(t *testing.T) {
	defer func() { syncFlags.pack, syncFlags.unpack = false, false }()
	copyCmd := newSyncCommand(syncjob.ModeCopy, "copy", "x")
	if err := copyCmd.ParseFlags([]string{"--pack", "--pack-threshold", "4M", "--pack-max", "512M"}); err != nil {
		t.Fatal(err)
	}
	var spec syncjob.Spec
	if err := applyPackFlags(&spec); err != nil {
		t.Fatal(err)
	}
	if p := spec.Pack; p == nil || !p.Enabled || p.Threshold != 4<<20 || p.MaxPackBytes != 512<<20 {
		t.Fatalf("pack = %+v, want 4M / 512M", spec.Pack)
	}

	// Unset flags show the defaults from syncjob.
	syncFlags.packThreshold = formatSize(syncjob.DefaultPackThreshold)
	syncFlags.packMax = formatSize(syncjob.DefaultPackMaxPackBytes)
	spec = syncjob.Spec{}
	_ = applyPackFlags(&spec)
	if spec.Pack.Threshold != 8<<20 || spec.Pack.MaxPackBytes != 1<<30 {
		t.Fatalf("default pack = %+v", spec.Pack)
	}

	syncFlags.unpack = true
	if err := applyPackFlags(&syncjob.Spec{}); err == nil {
		t.Fatalf("--pack with --unpack accepted")
	}
	syncFlags.pack = false
	spec = syncjob.Spec{}
	if err := applyPackFlags(&spec); err != nil || !spec.Unpack || spec.Pack != nil {
		t.Fatalf("--unpack: %+v, %v", spec, err)
	}
	syncFlags.unpack = false
	syncFlags.pack, syncFlags.packThreshold = true, "lots"
	if err := applyPackFlags(&syncjob.Spec{}); err == nil {
		t.Fatalf("bad threshold accepted")
	}
}

func TestPackFlagsOnlyOnCopyAndSync(t *testing.T) {
	for mode, ok := range map[syncjob.Mode]bool{syncjob.ModeCopy: true, syncjob.ModeSync: true, syncjob.ModeBisync: false, syncjob.ModeCheck: false} {
		c := newSyncCommand(mode, string(mode), "x")
		if has := c.Flags().Lookup("pack") != nil && c.Flags().Lookup("unpack") != nil; has != ok {
			t.Errorf("%s has pack flags = %v, want %v", mode, has, ok)
		}
	}
	var sync *cobra.Command
	for _, c := range RootCmd.Commands() {
		if c.Name() == "sync" {
			sync = c
		}
	}
	if sync == nil {
		t.Fatal("sync command not registered")
	}
	u, _, err := sync.Find([]string{"unpack", "a", "b"})
	if err != nil || u.Name() != "unpack" || u.Flags().Lookup("sync") == nil {
		t.Fatalf("sync unpack not found: %v %v", u, err)
	}
}

func TestJobsPrintsOnceUnlessFollowing(t *testing.T) {
	srv, _, polls, cancels := fakeServerCounting(t, syncjob.StateSucceeded, "")
	c := &syncClient{base: srv.URL, token: "tok", http: srv.Client()}
	var out bytes.Buffer
	if err := c.show(&out, "j1"); err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 1 || cancels.Load() != 0 {
		t.Fatalf("show polled %d times and cancelled %d times, want 1 and 0", polls.Load(), cancels.Load())
	}
	if !strings.Contains(out.String(), "j1") || !strings.Contains(out.String(), "running") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestShowReportsPackStageAndStats(t *testing.T) {
	var s syncjob.Snapshot
	if err := json.Unmarshal([]byte(`{"state":"running","stage":"packing","spec":{"pack":{"enabled":true}},"pack_stats":{"small_files":300,"packed_files":120,"packs_written":2,"packs_total":5,"bytes_in":2097152,"bytes_out":1048576,"dead_removed":4,"compacted_packs":1,"loose_removed":7,"conflicts":2,"v1_migrated_dirs":3}}`), &s); err != nil {
		t.Fatal(err)
	}
	if line := progressLine(s); !strings.Contains(line, "packing") {
		t.Fatalf("progress line %q doesn't show the packing stage", line)
	}
	pl := packLine(s)
	for _, want := range []string{"300 small files", "120 newly packed", "2.0 MiB read", "2 new packs", "1.0 MiB written", "5 packs in total", "4 entries retired", "1 packs compacted", "7 loose copies removed", "2 conflicts", "3 v1 dirs migrated"} {
		if !strings.Contains(pl, want) {
			t.Fatalf("pack line %q lacks %q", pl, want)
		}
	}
}

func TestInterruptWhileFollowingDoesNotCancelTheJob(t *testing.T) {
	srv, _, _, cancels := fakeServerCounting(t, syncjob.StateSucceeded, "")
	c := &syncClient{base: srv.URL, token: "tok", http: srv.Client()}
	ctx, interrupt := context.WithCancel(context.Background())
	interrupt() // Ctrl-C before the job ends (the fake needs three polls)
	if err := c.follow(ctx, "j1"); err != nil {
		t.Fatalf("follow after Ctrl-C: %v", err)
	}
	if cancels.Load() != 0 {
		t.Fatalf("Ctrl-C while following cancelled the job")
	}
}
