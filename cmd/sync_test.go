package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/syncjob"
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
	t.Helper()
	var started syncjob.Spec
	var polls atomic.Int32
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
		default:
			reply(w, 404, "not found", nil)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &started
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
	if err := c.follow("j1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
}

func TestClientReportsFailuresAndServerErrors(t *testing.T) {
	srv, _ := fakeServer(t, syncjob.StateFailed, "corrupted on transfer")
	c := &syncClient{base: srv.URL, token: "tok", http: srv.Client()}
	if _, err := c.start(syncjob.Spec{Mode: "mirror"}); err == nil || !strings.Contains(err.Error(), "mirror") {
		t.Fatalf("server-side rejection not surfaced: %v", err)
	}
	if err := c.follow("j1"); err == nil || !strings.Contains(err.Error(), "corrupted on transfer") {
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
