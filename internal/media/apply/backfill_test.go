package apply

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// tmdbWithBackdrops answers the detail of movie/1 (with a backdrop), movie/2
// (without one) and counts the requests.
func tmdbWithBackdrops(t *testing.T) (*scraper.Set, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/3/movie/1":
			_, _ = w.Write([]byte(`{"id":1,"title":"Inception","original_title":"Inception","release_date":"2010-07-16","poster_path":"/p1.jpg","backdrop_path":"/b1.jpg"}`))
		case "/3/movie/2":
			_, _ = w.Write([]byte(`{"id":2,"title":"Bare","original_title":"Bare","release_date":"2001-01-01","poster_path":"/p2.jpg"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	set, err := scraper.NewSet(scraper.Config{TMDBToken: "k", BaseURLs: map[string]string{"tmdb": srv.URL + "/3"}, RatePerSec: map[string]float64{"tmdb": 1000}})
	if err != nil {
		t.Fatal(err)
	}
	return set, &hits
}

// backdropWriter is a memWriter that also knows ids and backdrops.
type backdropWriter struct {
	*memWriter
	ids       map[string]map[string]string
	backdrops map[string]string
}

func (w *backdropWriter) Existing(ctx context.Context, prefix string) (map[string]Existing, error) {
	out, _ := w.memWriter.Existing(ctx, prefix)
	for p, e := range out {
		e.ExternalIDs, e.HasBackdrop = w.ids[p], w.backdrops[p] != ""
		out[p] = e
	}
	return out, nil
}

func (w *backdropWriter) Set(ctx context.Context, req model.MediaSetReq, force bool) error {
	if w.locked[req.Path] && !force {
		return ErrLocked
	}
	if req.Backdrop != nil {
		w.backdrops[req.Path] = *req.Backdrop
	}
	return w.memWriter.Set(ctx, req, force)
}

func imageOf(url string) ([]byte, error) { return []byte("img:" + url), nil }

// The scan's automatic scrape (no provider or id given) stores the poster and
// the backdrop; a locked item is left alone.
func TestAutoScrapeStoresBackdropAndSkipsLocked(t *testing.T) {
	set, _ := tmdbWithBackdrops(t)
	ls := &fakeLister{tree: map[string][]identify.Entry{
		"/lib/盗梦空间": {{Name: "Inception.2010.mkv", Size: 8 << 30}},
		"/lib/已锁定":  {{Name: "Inception.2010.mkv", Size: 8 << 30}},
	}}
	w := newMemWriter()
	w.items["/lib/已锁定"] = model.MediaSetReq{Path: "/lib/已锁定"}
	w.locked["/lib/已锁定"] = true
	cand := []scraper.Candidate{{Provider: "tmdb", ID: "movie/1", Title: "盗梦空间", Year: 2010, Score: 0.95}}
	o := &Orchestrator{
		Lister: ls, Writer: w, Providers: set,
		Search:   &fakeSearch{byTitle: map[string][]scraper.Candidate{"盗梦空间": cand, "已锁定": cand}},
		Download: func(_ context.Context, u string) ([]byte, error) { return imageOf(u) },
	}
	res, err := o.ScrapeOne(context.Background(), "/lib/盗梦空间", ScrapeOpts{})
	if err != nil || !res.Matched {
		t.Fatalf("scrape: %+v %v", res, err)
	}
	got := w.items["/lib/盗梦空间"]
	if got.Cover == nil || !strings.HasSuffix(*got.Cover, "-poster.jpg") || got.Backdrop == nil || !strings.HasSuffix(*got.Backdrop, "-backdrop.jpg") {
		t.Fatalf("cover/backdrop: %v %v", got.Cover, got.Backdrop)
	}
	if *got.Cover == *got.Backdrop {
		t.Fatalf("poster and backdrop are the same asset: %q", *got.Cover)
	}

	before := w.sets
	if _, err := o.ScrapeOne(context.Background(), "/lib/已锁定", ScrapeOpts{}); !errors.Is(err, ErrLocked) || w.sets != before {
		t.Fatalf("locked item: err %v, %d writes", err, w.sets-before)
	}
}

func TestBackfillFillsOnlyMissingBackdropsAndIsIdempotent(t *testing.T) {
	set, hits := tmdbWithBackdrops(t)
	w := &backdropWriter{memWriter: newMemWriter(), ids: map[string]map[string]string{}, backdrops: map[string]string{}}
	add := func(p, id string, locked bool, backdrop string) {
		kind := "movie"
		w.items[p] = model.MediaSetReq{Path: p, Kind: &kind}
		w.locked[p] = locked
		w.backdrops[p] = backdrop
		if id != "" {
			w.ids[p] = map[string]string{"tmdb": id}
		}
	}
	add("/lib/a", "movie/1", false, "")                    // filled
	add("/lib/b", "movie/1", false, "thumb:/imgs/own.jpg") // already has one
	add("/lib/c", "movie/1", true, "")                     // locked
	add("/lib/d", "", false, "")                           // never matched
	add("/lib/e", "movie/2", false, "")                    // TMDB has no backdrop
	o := &Orchestrator{Writer: w, Providers: set, Download: func(_ context.Context, u string) ([]byte, error) { return imageOf(u) }}

	rep, err := o.BackfillBackdrops(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 5 || rep.Filled != 1 || rep.Skipped != 4 || rep.Failed != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if !strings.HasSuffix(w.backdrops["/lib/a"], "-backdrop.jpg") || w.backdrops["/lib/b"] != "thumb:/imgs/own.jpg" || w.backdrops["/lib/c"] != "" || w.backdrops["/lib/e"] != "" {
		t.Fatalf("backdrops: %v", w.backdrops)
	}
	// only the backdrop is written: no title, source, lock or poster
	if r := w.items["/lib/a"]; r.Backdrop == nil || r.Title != nil || r.Source != nil || r.Locked != nil || r.Cover != nil {
		t.Fatalf("request wrote more than the backdrop: %+v", r)
	}
	if n := hits.Load(); n != 2 { // a and e; b, c, d never reach TMDB
		t.Fatalf("%d TMDB requests, want 2", n)
	}

	// again: a has one now, only e is asked (TMDB still has none)
	sets, uploads := w.sets, w.uploads
	rep, err = o.BackfillBackdrops(context.Background(), "/")
	if err != nil || rep.Filled != 0 || rep.Skipped != 5 || w.sets != sets || w.uploads != uploads || hits.Load() != 3 {
		t.Fatalf("second run: %+v err %v, %d writes, %d uploads, %d hits", rep, err, w.sets-sets, w.uploads-uploads, hits.Load())
	}
}

func TestBackfillWithoutDownloaderKeepsTheURL(t *testing.T) {
	set, _ := tmdbWithBackdrops(t)
	w := &backdropWriter{memWriter: newMemWriter(), ids: map[string]map[string]string{"/lib/a": {"tmdb": "movie/1"}}, backdrops: map[string]string{}}
	w.items["/lib/a"] = model.MediaSetReq{Path: "/lib/a"}
	o := &Orchestrator{Writer: w, Providers: set}
	if rep, err := o.BackfillBackdrops(context.Background(), "/"); err != nil || rep.Filled != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if got := w.backdrops["/lib/a"]; !strings.HasSuffix(got, "/w1280/b1.jpg") || !strings.HasPrefix(got, "http") {
		t.Fatalf("backdrop %q", got)
	}
}
