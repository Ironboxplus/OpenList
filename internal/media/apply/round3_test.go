package apply

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// captureSearch records the queries it is asked.
type captureSearch struct{ queries []scraper.IdentifyQuery }

func (c *captureSearch) Identify(_ context.Context, _ string, _ bool, q scraper.IdentifyQuery) ([]scraper.Candidate, error) {
	c.queries = append(c.queries, q)
	return nil, nil
}

func quentinTree() *fakeLister {
	return &fakeLister{tree: map[string][]identify.Entry{
		"/lib/昆汀合集":                     {{Name: "Pulp Fiction (1994)", IsDir: true}, {Name: "Kill Bill (2003)", IsDir: true}},
		"/lib/昆汀合集/Pulp Fiction (1994)": {{Name: "Pulp.Fiction.1994.mkv", Size: 8 << 30}},
		"/lib/昆汀合集/Kill Bill (2003)":    {{Name: "Kill.Bill.2003.mkv", Size: 8 << 30}},
	}}
}

// A batch scrape works from a list read before a rescan may have removed some
// rows; it must not bring a removed row back.
func TestScrapeOfARemovedItemDoesNotRecreateIt(t *testing.T) {
	w := newMemWriter()
	o := &Orchestrator{Lister: quentinTree(), Writer: w, Providers: &scraper.Set{}, Search: &captureSearch{}}
	_, err := o.ScrapeOne(context.Background(), "/lib/昆汀合集", ScrapeOpts{Existing: true})
	if !errors.Is(err, ErrGone) || w.sets != 0 {
		t.Fatalf("err %v, %d writes: want ErrGone and no write", err, w.sets)
	}
	// the same call for a row that is there writes it; so does a scrape by path
	w.items["/lib/昆汀合集"] = model.MediaSetReq{Path: "/lib/昆汀合集"}
	if _, err := o.ScrapeOne(context.Background(), "/lib/昆汀合集", ScrapeOpts{Existing: true}); err != nil || w.sets != 1 {
		t.Fatalf("present row: err %v, %d writes", err, w.sets)
	}
	w = newMemWriter()
	o.Writer = w
	if _, err := o.ScrapeOne(context.Background(), "/lib/昆汀合集", ScrapeOpts{}); err != nil || w.sets != 1 {
		t.Fatalf("scrape by path: err %v, %d writes", err, w.sets)
	}
}

// 与神同行/1 and /2 are looked up as the collection's title with their own
// number, year and names, not as "1" and "2".
func TestCollectionMemberWithoutATitleIsSearchedUnderTheCollectionTitle(t *testing.T) {
	coll := "collection"
	ls := &fakeLister{tree: map[string][]identify.Entry{
		"/Live/与神同行/2": {
			{Name: "Along.with.the.Gods.The.Last.49.Days.2018.BluRay.1080p.DTS-HD.MA.5.1.x265.10bit-BeiTai.mkv", Size: 12 << 30},
			{Name: "Along.with.the.Gods.The.Last.49.Days.2018.BluRay.1080p.DTS-HD.MA.5.1.x265.10bit-BeiTai.srt", Size: 121286},
		},
	}}
	w := newMemWriter()
	w.items["/Live/与神同行"] = model.MediaSetReq{Path: "/Live/与神同行", Kind: &coll}
	s := &captureSearch{}
	o := &Orchestrator{Lister: ls, Writer: w, Providers: &scraper.Set{}, Search: s}
	if _, err := o.ScrapeOne(context.Background(), "/Live/与神同行/2", ScrapeOpts{}); err != nil {
		t.Fatal(err)
	}
	if len(s.queries) == 0 {
		t.Fatal("nothing was searched")
	}
	q := s.queries[0]
	var names []string
	for _, tm := range q.Terms {
		names = append(names, tm.Name)
	}
	if !slices.Contains(names, "与神同行 2") || slices.Contains(names, "2") || q.Year != 2018 {
		t.Errorf("terms %v year %d", names, q.Year)
	}
	if len(q.Content) == 0 {
		t.Error("the names inside the member were not passed on")
	}

	// the same directory outside a collection keeps its own name
	w2 := newMemWriter()
	s2 := &captureSearch{}
	o = &Orchestrator{Lister: ls, Writer: w2, Providers: &scraper.Set{}, Search: s2}
	if _, err := o.ScrapeOne(context.Background(), "/Live/与神同行/2", ScrapeOpts{}); err != nil {
		t.Fatal(err)
	}
	for _, tm := range s2.queries[0].Terms {
		if tm.Name == "与神同行 2" {
			t.Errorf("a member of nothing took the parent title: %v", s2.queries[0].Terms)
		}
	}
}
