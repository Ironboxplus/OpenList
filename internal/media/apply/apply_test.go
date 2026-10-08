package apply

import (
	"errors"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

type fakeLister struct {
	mu    sync.Mutex
	tree  map[string][]identify.Entry
	calls []string
}

func (f *fakeLister) List(_ context.Context, p string) ([]identify.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p)
	es, ok := f.tree[p]
	if !ok {
		return nil, ErrNotDir
	}
	return es, nil
}

type memWriter struct {
	items   map[string]model.MediaSetReq
	locked  map[string]bool
	assets  map[string]uint
	sets    int
	uploads int
}

func newMemWriter() *memWriter {
	return &memWriter{items: map[string]model.MediaSetReq{}, locked: map[string]bool{}, assets: map[string]uint{}}
}

func (w *memWriter) Existing(_ context.Context, prefix string) (map[string]Existing, error) {
	out := map[string]Existing{}
	for p := range w.items {
		e := Existing{Path: p, Locked: w.locked[p]}
		if k := w.items[p].Kind; k != nil {
			e.Kind = *k
		}
		out[p] = e
	}
	return out, nil
}

func (w *memWriter) UploadAsset(_ context.Context, data []byte, _ string) (uint, error) {
	w.uploads++
	key := string(data)
	if id, ok := w.assets[key]; ok {
		return id, nil
	}
	id := uint(len(w.assets) + 1)
	w.assets[key] = id
	return id, nil
}

func (w *memWriter) Set(_ context.Context, req model.MediaSetReq, _ bool) error {
	w.sets++
	w.items[req.Path] = req
	if req.Locked != nil {
		w.locked[req.Path] = *req.Locked
	}
	return nil
}

// fakeSearch answers by title.
type fakeSearch struct {
	byTitle map[string][]scraper.Candidate
	queries int
}

func (f *fakeSearch) Identify(_ context.Context, kind string, _ bool, q scraper.IdentifyQuery) ([]scraper.Candidate, error) {
	f.queries++
	for _, t := range q.Terms {
		if cs, ok := f.byTitle[t.Name]; ok {
			return cs, nil
		}
	}
	return nil, nil
}

func videos(n int, pattern string) []identify.Entry {
	var es []identify.Entry
	for i := 1; i <= n; i++ {
		es = append(es, identify.Entry{Name: fmt.Sprintf(pattern, i), Size: 1 << 30})
	}
	return es
}

func TestPlanLooksOnlyWhereNeededAndNeverWrites(t *testing.T) {
	root := "/115/云下载"
	ls := &fakeLister{tree: map[string][]identify.Entry{
		root: {
			{Name: "盗梦空间", IsDir: true},
			{Name: "行尸走肉", IsDir: true},
			{Name: "已锁定", IsDir: true},
			{Name: "某种物质", IsDir: true},
			{Name: "昆汀合集", IsDir: true},
			{Name: "Fight Club (1999).mkv", Size: 4 << 30},
			{Name: "cover.jpg", Size: 1000},
		},
		root + "/盗梦空间":                     {{Name: "Inception.2010.mkv", Size: 8 << 30}},
		root + "/行尸走肉":                     {{Name: "Season 1", IsDir: true}, {Name: "Season 2", IsDir: true}},
		root + "/某种物质":                     {{Name: "The.Substance.2024.mkv", Size: 8 << 30}},
		root + "/昆汀合集":                     {{Name: "Pulp Fiction (1994)", IsDir: true}, {Name: "Kill Bill (2003)", IsDir: true}},
		root + "/昆汀合集/Pulp Fiction (1994)": {{Name: "Pulp.Fiction.1994.mkv", Size: 8 << 30}},
		root + "/昆汀合集/Kill Bill (2003)":    {{Name: "Kill.Bill.2003.mkv", Size: 8 << 30}},
	}}
	w := newMemWriter()
	w.items[root+"/已锁定"] = model.MediaSetReq{Path: root + "/已锁定"}
	w.locked[root+"/已锁定"] = true
	s := &fakeSearch{byTitle: map[string][]scraper.Candidate{
		"盗梦空间": {{Provider: "tmdb", ID: "movie/27205", Title: "盗梦空间", Year: 2010, Score: 0.93}},
		"行尸走肉": {{Provider: "tmdb", ID: "tv/1402", Title: "行尸走肉", Year: 2010, Score: 0.88}},
		"某种物质": {
			{Provider: "tmdb", ID: "movie/933260", Title: "某种物质", Year: 2024, Score: 0.41},
			{Provider: "tmdb", ID: "movie/2", Title: "物质", Year: 2001, Score: 0.35},
			{Provider: "tmdb", ID: "movie/3", Title: "Substance", Year: 1999, Score: 0.31},
			{Provider: "tmdb", ID: "movie/4", Title: "Fourth", Year: 1998, Score: 0.30},
		},
		"Fight Club": {{Provider: "tmdb", ID: "movie/550", Title: "搏击俱乐部", Year: 1999, Score: 0.9}},
	}}
	o := &Orchestrator{Lister: ls, Writer: w, Search: s}

	res, err := o.Plan(context.Background(), PlanOpts{Root: root, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if w.sets != 0 || w.uploads != 0 {
		t.Fatal("plan wrote something")
	}
	byPath := map[string]Row{}
	for _, r := range res.Rows {
		byPath[r.Path] = r
	}
	// a locked item is neither listed nor planned
	for _, c := range ls.calls {
		if strings.Contains(c, "已锁定") {
			t.Fatalf("a locked directory was listed: %v", ls.calls)
		}
	}
	if len(res.Skipped) != 1 || byPath[root+"/已锁定"].Path != "" {
		t.Fatalf("locked item planned/skipped wrongly: %v", res.Skipped)
	}
	// depth 1 does not descend into the collection's members
	for _, c := range ls.calls {
		if strings.Contains(c, "Pulp") || strings.Contains(c, "Kill") {
			t.Fatalf("depth 1 listed a collection member: %s", c)
		}
	}
	if len(res.Rows) != 5 {
		t.Fatalf("rows = %d (cover.jpg must be ignored): %+v", len(res.Rows), res.Rows)
	}
	if r := byPath[root+"/盗梦空间"]; r.Kind != "movie" || r.Provider != "tmdb" || r.ID != "movie/27205" || r.Confidence < 0.9 || r.Year != 2010 {
		t.Fatalf("row: %+v", r)
	}
	if r := byPath[root+"/行尸走肉"]; r.Kind != "tv" || r.ID != "tv/1402" {
		t.Fatalf("row: %+v", r)
	}
	if r := byPath[root+"/Fight Club (1999).mkv"]; r.Kind != "movie" || r.ID != "movie/550" {
		t.Fatalf("file row: %+v", r)
	}
	if r := byPath[root+"/昆汀合集"]; r.Kind != "collection" || r.Provider != "" {
		t.Fatalf("collection row: %+v", r)
	}
	// below 0.6 the note carries the top three candidates
	low := byPath[root+"/某种物质"]
	if low.Confidence >= 0.6 {
		t.Fatalf("low row confidence %v", low.Confidence)
	}
	if n := strings.Count(low.Note, "tmdb:"); n != 3 || !strings.Contains(low.Note, "tmdb:movie/933260 某种物质 2024 | tmdb:movie/2") || strings.Contains(low.Note, "movie/4") {
		t.Fatalf("note = %q", low.Note)
	}

	// depth 2 plans the members of the collection
	ls.calls = nil
	res, err = o.Plan(context.Background(), PlanOpts{Root: root, Depth: 2})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, r := range res.Rows {
		if strings.HasPrefix(r.Path, root+"/昆汀合集/") {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("collection members at depth 2: %d", found)
	}
}

func TestPlanStopsOnNetworkFailure(t *testing.T) {
	ls := &fakeLister{tree: map[string][]identify.Entry{
		"/r":   {{Name: "A", IsDir: true}, {Name: "B", IsDir: true}},
		"/r/A": {{Name: "a.mkv", Size: 1 << 30}},
		"/r/B": {{Name: "b.mkv", Size: 1 << 30}},
	}}
	o := &Orchestrator{Lister: ls, Writer: newMemWriter(), Search: failingSearch{}}
	res, err := o.Plan(context.Background(), PlanOpts{Root: "/r"})
	if scraper.ClassOf(err) != scraper.ClassNetwork {
		t.Fatalf("err = %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("the plan went on after a network failure: %d rows", len(res.Rows))
	}
}

type failingSearch struct{}

func (failingSearch) Identify(context.Context, string, bool, scraper.IdentifyQuery) ([]scraper.Candidate, error) {
	return nil, &scraper.Error{Provider: "tmdb", Class: scraper.ClassNetwork, Msg: "unreachable"}
}

func TestTSVRoundTripAndValidation(t *testing.T) {
	rows := []Row{
		{Path: "/a/b", Kind: "tv", Provider: "tmdb", ID: "tv/1", Title: "T\twith tab", Year: 2001, Confidence: 0.87, Note: "n"},
		{Path: "/a/c", Kind: "ignore"},
	}
	var buf bytes.Buffer
	if err := FormatTSV(&buf, rows); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "#path\tkind\tprovider\tid\ttitle\tyear\tconfidence\tnote\n") {
		t.Fatalf("header: %q", buf.String())
	}
	got, err := ParseTSV(&buf)
	if err != nil || len(got) != 2 {
		t.Fatalf("parse: %v %+v", err, got)
	}
	if got[0].Title != "T with tab" || got[0].Year != 2001 || got[0].Confidence != 0.87 || got[1].Kind != "ignore" {
		t.Fatalf("rows: %+v", got)
	}
	for _, bad := range []string{"relative\tmovie\n", "/a\tbogus\n", "/a\tmovie\ttmdb\n", "/a\tmovie\t\t\t\tnotayear\n"} {
		if _, err := ParseTSV(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestApplyDryRunWritesNothingAndRerunIsIdempotent(t *testing.T) {
	w := newMemWriter()
	o := &Orchestrator{Writer: w}
	rows := []Row{{Path: "/a/x", Kind: "movie", Title: "X", Year: 2000}, {Path: "/a/y", Kind: "ignore"}}

	rep, err := o.Apply(context.Background(), rows, ApplyOpts{DryRun: true})
	if err != nil || rep.Applied != 2 || w.sets != 0 || w.uploads != 0 {
		t.Fatalf("dry run: %+v %v sets=%d", rep, err, w.sets)
	}
	if _, err := o.Apply(context.Background(), rows, ApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	first := dump(w.items)
	if _, err := o.Apply(context.Background(), rows, ApplyOpts{}); err != nil {
		t.Fatal(err)
	}
	if dump(w.items) != first || len(w.items) != 2 {
		t.Fatalf("second run changed the result:\n%s\n%v", first, w.items)
	}
	x := w.items["/a/x"]
	if x.Locked == nil || !*x.Locked || *x.Source != "manual" {
		t.Fatalf("reviewed rows must be written locked and manual: %+v", x)
	}
	// --no-lock
	if _, err := o.Apply(context.Background(), rows[:1], ApplyOpts{NoLock: true}); err != nil {
		t.Fatal(err)
	}
	if *w.items["/a/x"].Locked {
		t.Fatal("NoLock still locked")
	}
}

func TestApplyWithoutKeysAsksTheServer(t *testing.T) {
	w := &scrapingWriter{memWriter: newMemWriter()}
	o := &Orchestrator{Writer: w}
	rep, err := o.Apply(context.Background(), []Row{{Path: "/a/x", Kind: "tv", Provider: "tmdb", ID: "tv/890", Title: "EVA"}}, ApplyOpts{})
	if err != nil || rep.Applied != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if len(w.scraped) != 1 || w.scraped[0] != "/a/x tmdb tv/890 lock=true force=true" {
		t.Fatalf("server scrape calls: %v", w.scraped)
	}
	// and with a plain writer the missing keys are a clear failure, not a silent skip
	o = &Orchestrator{Writer: newMemWriter()}
	rep, _ = o.Apply(context.Background(), []Row{{Path: "/a/x", Kind: "tv", Provider: "tmdb", ID: "tv/890"}}, ApplyOpts{})
	if rep.Failed != 1 || !strings.Contains(rep.Lines[0], "--env") {
		t.Fatalf("%+v", rep)
	}
}

type scrapingWriter struct {
	*memWriter
	scraped []string
}

func (s *scrapingWriter) ServerScrape(_ context.Context, p, provider, id string, lock, force bool) error {
	s.scraped = append(s.scraped, fmt.Sprintf("%s %s %s lock=%v force=%v", p, provider, id, lock, force))
	return nil
}

func dump(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestLibraryRootsAreNotPlannedOrScraped(t *testing.T) {
	root := "/115/arc/MOV"
	ls := &fakeLister{tree: map[string][]identify.Entry{
		root:                {{Name: "Anime", IsDir: true}, {Name: "盗梦空间", IsDir: true}},
		root + "/盗梦空间": {{Name: "Inception.2010.mkv", Size: 8 << 30}},
	}}
	w := newMemWriter()
	lib := "library"
	w.items[root+"/Anime"] = model.MediaSetReq{Path: root + "/Anime", Kind: &lib} // unlocked on purpose
	o := &Orchestrator{Lister: ls, Writer: w}
	res, err := o.Plan(context.Background(), PlanOpts{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Path != root+"/盗梦空间" || len(res.Skipped) != 1 {
		t.Fatalf("rows=%+v skipped=%v", res.Rows, res.Skipped)
	}
	for _, c := range ls.calls {
		if c == root+"/Anime" {
			t.Error("the library directory was listed by plan")
		}
	}
	o.Providers = &scraper.Set{}
	if _, err := o.ScrapeOne(context.Background(), root+"/Anime", ScrapeOpts{Force: true}); !errors.Is(err, ErrLibrary) {
		t.Fatalf("scraping a library root: %v", err)
	}
	// a plan row of kind library is accepted and written as is, without a fetch
	rows, err := ParseTSV(strings.NewReader("/115/arc/MOV/Live\tlibrary\n"))
	if err != nil || len(rows) != 1 {
		t.Fatalf("tsv: %v %+v", err, rows)
	}
	rep, err := o.Apply(context.Background(), rows, ApplyOpts{})
	if err != nil || rep.Failed != 0 || *w.items[root+"/Live"].Kind != "library" {
		t.Fatalf("apply: %v %+v", err, rep)
	}
}
