package noindex

import (
	"context"
	"fmt"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/search/searcher"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:noindex-test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	conf.Conf = conf.DefaultConfig("data")
	db.Init(dB)
}

func TestMatchKeywords(t *testing.T) {
	cases := []struct {
		name     string
		keywords []string
		want     bool
	}{
		{"Death.End.Cursed.2024.mkv", []string{"death", "cursed"}, true},
		{"Death.End.Cursed.2024.mkv", []string{"Death", "CURSED"}, true}, // case-insensitive
		{"Death.End.Cursed.2024.mkv", []string{"death", "missing"}, false},
		{"holiday photos", nil, true}, // no keywords matches everything
		{"holiday photos", []string{}, true},
		{"a.txt", []string{"a", "txt"}, true},
		{"a.txt", []string{"b"}, false},
	}
	for _, c := range cases {
		if got := matchKeywords(c.name, c.keywords); got != c.want {
			t.Fatalf("matchKeywords(%q, %v) = %v, want %v", c.name, c.keywords, got, c.want)
		}
	}
}

func TestSkipDriver(t *testing.T) {
	list := splitList(defaultSkipDrivers)
	for name, want := range map[string]bool{
		"115 Cloud": true, "115 Open": true, "115 share": true, // case-insensitive
		"Local": false, "FakeCounting": false, "": false,
	} {
		if got := skipDriver(name, list); got != want {
			t.Errorf("skipDriver(%q) = %v, want %v", name, got, want)
		}
	}
	if skipDriver("115 Cloud", splitList("")) {
		t.Error("an empty skip list must skip nothing")
	}
	if got := splitList(" a , ,b "); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("splitList = %v", got)
	}
}

// mount creates a fake storage and tears it down with the test.
func mount(t *testing.T, mountPath string, disableIndex bool) *fakedriver.Driver {
	t.Helper()
	id, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: fakedriver.DriverName, MountPath: mountPath, Addition: "{}", CacheExpiration: 30, DisableIndex: disableIndex,
	})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	s, err := op.GetStorageByMountPath(mountPath)
	if err != nil {
		t.Fatal(err)
	}
	return s.(*fakedriver.Driver)
}

func testLimits() limits {
	return limits{maxDepth: 20, maxVisited: 100000, timeout: 60e9, maxConcurrent: 100}
}

func req(parent, keywords string) model.SearchReq {
	return model.SearchReq{Parent: parent, Keywords: keywords, Page: 1, PerPage: 100}
}

func names(nodes []model.SearchNode) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.Parent+"/"+n.Name)
	}
	return out
}

func TestSearchWalksAndMatches(t *testing.T) {
	d := mount(t, "/ni-walk", false)
	d.AddDir("/docs")
	d.AddFile("/docs/Report.PDF", "x")
	d.AddFile("/docs/notes.txt", "x")
	d.AddDir("/docs/report-archive")
	d.AddFile("/docs/report-archive/old-report.txt", "x")
	d.AddFile("/other.txt", "x")

	nodes, total, err := search(context.Background(), req("/ni-walk", "REPORT"), nil, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/ni-walk/docs//report-archive", "/ni-walk/docs/report-archive/old-report.txt", "/ni-walk/docs/Report.PDF"}
	_ = want
	if total != 3 || len(nodes) != 3 {
		t.Fatalf("got %v total %d, want 3 matches", names(nodes), total)
	}

	// scope 1 = directories only, 2 = files only
	r := req("/ni-walk", "report")
	r.Scope = 1
	nodes, _, _ = search(context.Background(), r, nil, testLimits())
	if len(nodes) != 1 || !nodes[0].IsDir {
		t.Fatalf("scope 1: %v", names(nodes))
	}
	r.Scope = 2
	nodes, _, _ = search(context.Background(), r, nil, testLimits())
	if len(nodes) != 2 {
		t.Fatalf("scope 2: %v", names(nodes))
	}

	// the parent itself is never a result, and pagination slices the sorted list
	r = req("/ni-walk/docs", "")
	r.PerPage = 2
	r.Page = 2
	nodes, total, _ = search(context.Background(), r, nil, testLimits())
	if total != 4 || len(nodes) != 2 {
		t.Fatalf("page 2: %v total %d", names(nodes), total)
	}
	r.Page = 3
	nodes, total, _ = search(context.Background(), r, nil, testLimits())
	if total != 4 || len(nodes) != 0 {
		t.Fatalf("page 3: %v total %d", names(nodes), total)
	}
}

func TestSearchIgnorePaths(t *testing.T) {
	d := mount(t, "/ni-ign", false)
	d.AddDir("/skip")
	d.AddFile("/skip/hit.txt", "x")
	d.AddFile("/hit.txt", "x")
	lim := testLimits()
	lim.ignorePaths = []string{"/ni-ign/skip"}
	nodes, _, err := search(context.Background(), req("/ni-ign", "hit"), nil, lim)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Parent != "/ni-ign" {
		t.Fatalf("got %v", names(nodes))
	}
}

func TestSearchDoesNotListDisabledOrSkippedStorages(t *testing.T) {
	off := mount(t, "/ni-off", true)
	off.AddFile("/hit.txt", "x")
	off.ResetCounts()
	nodes, _, err := search(context.Background(), req("/ni-off", "hit"), nil, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 || off.Lists.Load() != 0 {
		t.Fatalf("disable_index storage: nodes %v, lists %d, want none and 0", names(nodes), off.Lists.Load())
	}

	// a driver on the skip list is not walked either (stands in for 115)
	skipped := mount(t, "/ni-skip", false)
	skipped.AddFile("/hit.txt", "x")
	skipped.ResetCounts()
	lim := testLimits()
	lim.skipDrivers = []string{fakedriver.DriverName}
	nodes, _, err = search(context.Background(), req("/ni-skip", "hit"), nil, lim)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 || skipped.Lists.Load() != 0 {
		t.Fatalf("skipped driver: nodes %v, lists %d, want none and 0", names(nodes), skipped.Lists.Load())
	}

	// and walking from a parent above the mount skips the subtree as well
	nodes, _, err = search(context.Background(), req("/", "hit"), nil, lim)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Parent == "/ni-skip" || n.Parent == "/ni-off" {
			t.Fatalf("walk from / entered a skipped storage: %v", names(nodes))
		}
	}
	if skipped.Lists.Load() != 0 || off.Lists.Load() != 0 {
		t.Fatalf("walk from / listed skipped storages (%d, %d)", skipped.Lists.Load(), off.Lists.Load())
	}
}

func TestSearchStopsAtVisitedBudget(t *testing.T) {
	d := mount(t, "/ni-budget", false)
	for i := 0; i < 50; i++ {
		d.AddFile(fmt.Sprintf("/f%02d.txt", i), "x")
	}
	lim := testLimits()
	lim.maxVisited = 10
	nodes, total, err := search(context.Background(), req("/ni-budget", "f"), nil, lim)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || total > 10 {
		t.Fatalf("budget 10 returned %d results (%v)", total, names(nodes))
	}
}

func TestSearchStopsWhenContextIsDone(t *testing.T) {
	d := mount(t, "/ni-done", false)
	d.AddDir("/a")
	d.AddFile("/a/hit.txt", "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.ResetCounts()
	nodes, _, err := search(ctx, req("/ni-done", "hit"), nil, testLimits())
	if err == nil && len(nodes) != 0 {
		t.Fatalf("cancelled search returned %v", names(nodes))
	}
	if d.Lists.Load() != 0 {
		t.Fatalf("cancelled search listed the storage %d times", d.Lists.Load())
	}
}

// The filter must run before the result cap: entries the caller may not see
// must not use up the budget.
func TestSearchFiltersBeforeCap(t *testing.T) {
	old := maxResults
	maxResults = 3
	t.Cleanup(func() { maxResults = old })

	d := mount(t, "/ni-filter", false)
	for i := 0; i < 5; i++ {
		d.AddFile(fmt.Sprintf("/a-secret-%d.txt", i), "x") // listed first
	}
	d.AddFile("/b-ok-1.txt", "x")
	d.AddFile("/b-ok-2.txt", "x")

	visible := func(n model.SearchNode) bool { return n.Name[:8] != "a-secret" }
	nodes, total, err := search(context.Background(), req("/ni-filter", ".txt"), visible, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(nodes) != 2 {
		t.Fatalf("got %v total %d, want the 2 visible files even though 5 hidden ones come first", names(nodes), total)
	}
	for _, n := range nodes {
		if !visible(n) {
			t.Fatalf("hidden entry leaked: %v", n)
		}
	}

	// without a filter the cap applies to everything
	nodes, total, _ = search(context.Background(), req("/ni-filter", ".txt"), nil, testLimits())
	if total != 3 || len(nodes) != 3 {
		t.Fatalf("unfiltered: %v total %d, want cap 3", names(nodes), total)
	}
}

func TestSearchRefusesWhenTooManyAreRunning(t *testing.T) {
	d := mount(t, "/ni-busy", false)
	d.AddFile("/hit.txt", "x")
	lim := testLimits()
	lim.maxConcurrent = 2
	running.Store(2)
	t.Cleanup(func() { running.Store(0) })
	if _, _, err := search(context.Background(), req("/ni-busy", "hit"), nil, lim); err != ErrBusy {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if got := running.Load(); got != 2 {
		t.Fatalf("a refused search must not change the counter, got %d", got)
	}
	running.Store(1)
	if _, _, err := search(context.Background(), req("/ni-busy", "hit"), nil, lim); err != nil {
		t.Fatalf("second slot should be free: %v", err)
	}
	if got := running.Load(); got != 1 {
		t.Fatalf("counter after a finished search = %d, want 1", got)
	}
}

func TestIndexMethodsAreNoOps(t *testing.T) {
	n := NoIndex{}
	ctx := context.Background()
	if n.Config().AutoUpdate || n.Config().Name != "no_index" {
		t.Fatalf("config = %+v", n.Config())
	}
	if n.Index(ctx, model.SearchNode{}) != nil || n.BatchIndex(ctx, nil) != nil || n.Del(ctx, "/") != nil || n.Clear(ctx) != nil || n.Release(ctx) != nil {
		t.Fatal("index mutations must be no-ops")
	}
	if got, err := n.Get(ctx, "/"); err != nil || len(got) != 0 {
		t.Fatalf("Get = %v, %v", got, err)
	}
}

func TestRegisteredAsFilteredSearcher(t *testing.T) {
	mk, ok := searcher.NewMap["no_index"]
	if !ok {
		t.Fatal("no_index is not registered")
	}
	s, err := mk()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(searcher.FilteredSearcher); !ok {
		t.Fatal("no_index must implement searcher.FilteredSearcher so search.SearchFiltered filters inside the walk")
	}
}
