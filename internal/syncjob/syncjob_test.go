package syncjob_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/local"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/syncjob"
	"github.com/glebarez/sqlite"
	"github.com/rclone/rclone/fs/filter"
	"gorm.io/gorm"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:syncjob_test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig(os.TempDir())
	db.Init(dB)
}

func mountLocal(t *testing.T, mountPath string) string {
	t.Helper()
	dir := t.TempDir()
	id, err := op.CreateStorage(context.Background(), model.Storage{
		Driver:    "Local",
		MountPath: mountPath,
		Addition:  `{"root_folder_path":"` + strings.ReplaceAll(dir, `\`, `\\`) + `"}`,
	})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	return dir
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func wait(t *testing.T, m *syncjob.Manager, id string) syncjob.Snapshot {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s, ok := m.Get(id, false)
		if !ok {
			t.Fatalf("job %s vanished", id)
		}
		if s.State != syncjob.StateRunning {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return syncjob.Snapshot{}
}

func newManager(t *testing.T) *syncjob.Manager {
	m := syncjob.NewManager()
	m.SetHistoryPath(filepath.Join(t.TempDir(), "sync-jobs.jsonl"))
	return m
}

func runJob(t *testing.T, m *syncjob.Manager, spec syncjob.Spec) syncjob.Snapshot {
	t.Helper()
	s, err := m.Start(spec)
	if err != nil {
		t.Fatalf("Start(%+v): %v", spec, err)
	}
	return wait(t, m, s.ID)
}

func TestSyncFromLocalDirToMountAndCheck(t *testing.T) {
	m := newManager(t)
	src := t.TempDir()
	dst := mountLocal(t, "/job-sync")
	write(t, src, "a.txt", "alpha")
	write(t, src, "skip.tmp", "temp")
	write(t, src, "d/b.txt", "bravo")

	s := runJob(t, m, syncjob.Spec{Mode: syncjob.ModeSync, Src: "local:" + src, Dst: "/job-sync", Filters: []string{"- *.tmp"}})
	if s.State != syncjob.StateSucceeded {
		t.Fatalf("sync state = %s, error = %s", s.State, s.Error)
	}
	if got, _ := read(t, dst, "d/b.txt"); got != "bravo" {
		t.Fatalf("d/b.txt = %q", got)
	}
	if _, ok := read(t, dst, "skip.tmp"); ok {
		t.Fatalf("filtered file was synced")
	}
	// Uploads range-read the source instead of reading rclone's stream; the
	// bytes must still show up in the job's progress.
	if b, _ := s.Stats["bytes"].(int64); b < int64(len("alpha")+len("bravo")) {
		t.Fatalf("stats bytes = %v, want at least the uploaded content (stats: %+v)", s.Stats["bytes"], s.Stats)
	}

	// The Local driver reports no hashes, so this storage compares by size;
	// hash comparison against a storage that has them is covered in rclonefs.
	write(t, src, "a.txt", "alpha, edited")
	if c := runJob(t, m, syncjob.Spec{Mode: syncjob.ModeCheck, Src: "local:" + src, Dst: "/job-sync", Filters: []string{"- *.tmp"}}); c.State != syncjob.StateFailed {
		t.Fatalf("check after an edit = %s, want failed", c.State)
	}
	if s := runJob(t, m, syncjob.Spec{Mode: syncjob.ModeSync, Src: "local:" + src, Dst: "/job-sync", Filters: []string{"- *.tmp"}}); s.State != syncjob.StateSucceeded {
		t.Fatalf("second sync: %s %s", s.State, s.Error)
	}
	if got, _ := read(t, dst, "a.txt"); got != "alpha, edited" {
		t.Fatalf("edit not propagated: a.txt = %q", got)
	}
	if c := runJob(t, m, syncjob.Spec{Mode: syncjob.ModeCheck, Src: "local:" + src, Dst: "/job-sync", Filters: []string{"- *.tmp"}}); c.State != syncjob.StateSucceeded {
		t.Fatalf("check after sync = %s: %s", c.State, c.Error)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	m := newManager(t)
	src := t.TempDir()
	dst := mountLocal(t, "/job-dry")
	write(t, src, "a.txt", "alpha")
	if s := runJob(t, m, syncjob.Spec{Mode: syncjob.ModeCopy, Src: "local:" + src, Dst: "/job-dry", DryRun: true}); s.State != syncjob.StateSucceeded {
		t.Fatalf("dry run: %s %s", s.State, s.Error)
	}
	if _, ok := read(t, dst, "a.txt"); ok {
		t.Fatalf("dry run wrote a.txt")
	}
}

func TestCopyBetweenTwoMounts(t *testing.T) {
	m := newManager(t)
	a := mountLocal(t, "/job-a")
	b := mountLocal(t, "/job-b")
	write(t, a, "x/y.txt", "yankee")
	if s := runJob(t, m, syncjob.Spec{Mode: syncjob.ModeCopy, Src: "/job-a", Dst: "/job-b"}); s.State != syncjob.StateSucceeded {
		t.Fatalf("copy: %s %s", s.State, s.Error)
	}
	if got, _ := read(t, b, "x/y.txt"); got != "yankee" {
		t.Fatalf("x/y.txt = %q", got)
	}
}

func TestBisyncPropagatesBothWays(t *testing.T) {
	flags.DataDir = t.TempDir()
	m := newManager(t)
	local := t.TempDir()
	remote := mountLocal(t, "/job-bi")
	write(t, local, "l.txt", "from local")
	write(t, remote, "r.txt", "from remote")

	spec := syncjob.Spec{Mode: syncjob.ModeBisync, Src: "local:" + local, Dst: "/job-bi"}
	if s := runJob(t, m, spec); s.State != syncjob.StateFailed {
		t.Fatalf("first bisync without resync = %s, want failed (rclone requires --resync first)", s.State)
	}
	spec.Resync = true
	if s := runJob(t, m, spec); s.State != syncjob.StateSucceeded {
		t.Fatalf("resync: %s %s", s.State, s.Error)
	}
	if got, _ := read(t, local, "r.txt"); got != "from remote" {
		t.Fatalf("resync did not bring r.txt down: %q", got)
	}
	if got, _ := read(t, remote, "l.txt"); got != "from local" {
		t.Fatalf("resync did not push l.txt up: %q", got)
	}

	spec.Resync = false
	write(t, local, "l2.txt", "new local")
	write(t, remote, "r2.txt", "new remote")
	if s := runJob(t, m, spec); s.State != syncjob.StateSucceeded {
		t.Fatalf("bisync: %s %s", s.State, s.Error)
	}
	if got, _ := read(t, remote, "l2.txt"); got != "new local" {
		t.Fatalf("l2.txt not pushed: %q", got)
	}
	if got, _ := read(t, local, "r2.txt"); got != "new remote" {
		t.Fatalf("r2.txt not pulled: %q", got)
	}
}

func TestStartRejectsBadRequests(t *testing.T) {
	m := newManager(t)
	mountLocal(t, "/job-bad")
	for _, spec := range []syncjob.Spec{
		{Mode: "mirror", Src: "/job-bad", Dst: "/job-bad"},
		{Mode: syncjob.ModeCopy, Src: "local:relative/dir", Dst: "/job-bad"},
		{Mode: syncjob.ModeCopy, Src: "/job-bad", Dst: "no-slash"},
		{Mode: syncjob.ModeCopy, Src: "/not-mounted-anywhere", Dst: "/job-bad"},
	} {
		if _, err := m.Start(spec); err == nil {
			t.Errorf("Start(%+v) accepted a bad request", spec)
		}
	}
}

func TestCancelStopsAJob(t *testing.T) {
	m := newManager(t)
	src := t.TempDir()
	mountLocal(t, "/job-cancel")
	for i := 0; i < 200; i++ {
		write(t, src, filepath.Join("many", strings.Repeat("f", 1+i%5)+string(rune('a'+i%26))+".txt"), strings.Repeat("z", 1000))
	}
	s, err := m.Start(syncjob.Spec{Mode: syncjob.ModeCopy, Src: "local:" + src, Dst: "/job-cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Cancel(s.ID) {
		t.Fatalf("Cancel(%s) = false", s.ID)
	}
	if got := wait(t, m, s.ID); got.State != syncjob.StateCanceled && got.State != syncjob.StateSucceeded {
		t.Fatalf("canceled job state = %s (%s)", got.State, got.Error)
	}
	if m.Cancel("nope") {
		t.Fatalf("Cancel of an unknown id reported success")
	}
}

func TestJobIsObservable(t *testing.T) {
	m := newManager(t)
	src := t.TempDir()
	mountLocal(t, "/job-obs")
	write(t, src, "a.txt", "alpha")
	write(t, src, "b.txt", "bravo")
	spec := syncjob.Spec{Mode: syncjob.ModeSync, Src: "local:" + src, Dst: "/job-obs"}
	s := runJob(t, m, spec)
	if s.State != syncjob.StateSucceeded {
		t.Fatalf("%s %s", s.State, s.Error)
	}
	if s.Counters.OnlyInSrc != 2 || s.Counters.Uploaded != 2 || s.Counters.UploadedBytes != 10 {
		t.Fatalf("counters after first sync = %+v", s.Counters)
	}
	full, _ := m.Get(s.ID, true)
	kinds := map[string]int{}
	for _, e := range full.Events {
		kinds[e.Kind]++
	}
	if kinds["uploaded"] != 2 || kinds["only_in_src"] != 2 {
		t.Fatalf("events = %+v", full.Events)
	}

	// An unchanged second run only matches; nothing is logged per file.
	s2 := runJob(t, m, spec)
	if s2.Counters.Match != 2 || s2.Counters.Uploaded != 0 {
		t.Fatalf("counters on an unchanged run = %+v", s2.Counters)
	}
	if e, _ := m.Get(s2.ID, true); len(e.Events) != 0 {
		t.Fatalf("matching files were logged as events: %+v", e.Events)
	}

	// Finished jobs survive a restart through the history file.
	hist, err := m.History(10)
	if err != nil || len(hist) != 2 || hist[0].ID != s2.ID || hist[1].Counters.Uploaded != 2 {
		t.Fatalf("history = %+v, %v", hist, err)
	}
}

func TestMaxDeleteStopsASyncThatWouldDeleteTooMuch(t *testing.T) {
	m := newManager(t)
	src := t.TempDir()
	dst := mountLocal(t, "/job-maxdel")
	write(t, dst, "keep-me.txt", "precious")
	zero := int64(0)
	s := runJob(t, m, syncjob.Spec{Mode: syncjob.ModeSync, Src: "local:" + src, Dst: "/job-maxdel", MaxDelete: &zero})
	if s.State != syncjob.StateFailed {
		t.Fatalf("sync with max_delete=0 that needs a delete = %s, want failed", s.State)
	}
	if _, ok := read(t, dst, "keep-me.txt"); !ok {
		t.Fatalf("file deleted despite max_delete=0")
	}
}

func TestBandwidthLimitCanBeSetAndCleared(t *testing.T) {
	t.Cleanup(func() { _ = syncjob.SetBwLimit("off") })
	if err := syncjob.SetBwLimit("2M"); err != nil {
		t.Fatal(err)
	}
	if got, err := syncjob.BwLimit(); err != nil || got != 2*1024*1024 {
		t.Fatalf("BwLimit after 2M = %d, %v", got, err)
	}
	if err := syncjob.SetBwLimit("off"); err != nil {
		t.Fatal(err)
	}
	if got, _ := syncjob.BwLimit(); got != -1 {
		t.Fatalf("BwLimit after off = %d", got)
	}
	if err := syncjob.SetBwLimit("fast please"); err == nil {
		t.Fatalf("nonsense rate accepted")
	}
}

func TestStartRejectsBadControls(t *testing.T) {
	m := newManager(t)
	mountLocal(t, "/job-ctl")
	for _, spec := range []syncjob.Spec{
		{Mode: syncjob.ModeCopy, Src: "/job-ctl", Dst: "/job-ctl", Transfers: -1},
		{Mode: syncjob.ModeCopy, Src: "/job-ctl", Dst: "/job-ctl", Filters: []string{"? not a rule"}},
	} {
		if _, err := m.Start(spec); err == nil {
			t.Errorf("Start(%+v) accepted", spec)
		}
	}
}

func TestNewFilterOptUsesRcloneDefaults(t *testing.T) {
	got := syncjob.NewFilterOpt([]string{"- *.tmp"})
	if got.MinAge != filter.Opt.MinAge || got.MaxAge != filter.Opt.MaxAge {
		t.Fatalf("ages = %v/%v, want rclone defaults %v/%v", got.MinAge, got.MaxAge, filter.Opt.MinAge, filter.Opt.MaxAge)
	}
	if len(got.FilterRule) != 1 {
		t.Fatalf("FilterRule not set: %v", got.FilterRule)
	}
	if _, err := filter.NewFilter(&got); err != nil {
		t.Fatal(err)
	}
	// The pre-fix zero value is what fails on clocks that tick between reads.
	zero := filter.Options{}
	if zero.MaxAge == filter.Opt.MaxAge {
		t.Fatal("zero Options unexpectedly equals defaults")
	}
}
