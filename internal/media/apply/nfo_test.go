package apply

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
)

// nfoLister is a tree of directories whose files can be read.
type nfoLister struct {
	dirs  map[string][]identify.Entry
	files map[string]string
	reads []string
}

func (l *nfoLister) List(_ context.Context, p string) ([]identify.Entry, error) {
	if es, ok := l.dirs[p]; ok {
		return es, nil
	}
	return nil, ErrNotDir
}

func (l *nfoLister) ReadFile(_ context.Context, p string, _ int64) ([]byte, error) {
	l.reads = append(l.reads, p)
	return []byte(l.files[p]), nil
}

// tmdbKnowing answers the existence check of the given TMDB ids ("movie/27205").
func tmdbKnowing(t *testing.T, known ...string) *scraper.Set {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, k := range known {
			if r.URL.Path == "/3/"+k {
				_, _ = w.Write([]byte(`{"id":1}`))
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	set, err := scraper.NewSet(scraper.Config{TMDBToken: "k", BaseURLs: map[string]string{"tmdb": srv.URL + "/3"}})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestNFONextToAWorkNamesItWithoutASearch(t *testing.T) {
	l := &nfoLister{
		dirs: map[string][]identify.Entry{
			"/lib/盗梦":  {{Name: "movie.nfo", Size: 400}, {Name: "x.mkv", Size: 1 << 30}},
			"/lib/剧":   {{Name: "tvshow.nfo", Size: 400}, {Name: "e01.mkv", Size: 1 << 30}},
			"/lib":     {{Name: "盗梦", IsDir: true}, {Name: "Film (2001).mkv", Size: 1 << 30}, {Name: "Film (2001).nfo", Size: 100}},
			"/lib/空目录": {{Name: "x.mkv", Size: 1 << 30}},
		},
		files: map[string]string{
			"/lib/盗梦/movie.nfo":     `<movie><uniqueid type="tmdb">27205</uniqueid></movie>`,
			"/lib/剧/tvshow.nfo":     `<tvshow><tmdbid>1399</tmdbid></tvshow>`,
			"/lib/Film (2001).nfo":  "https://www.imdb.com/title/tt0000000/",
			"/lib/盗梦/movie.nfo.bad": "",
		},
	}
	o := &Orchestrator{Lister: l, Providers: tmdbKnowing(t, "movie/27205", "tv/1399")}
	ctx := context.Background()

	id, ok := o.idFromNFO(ctx, "/lib/盗梦", "盗梦", true, l.dirs["/lib/盗梦"], identify.Result{Kind: "movie"})
	if !ok || id != "movie/27205" {
		t.Fatalf("movie.nfo: %q %v", id, ok)
	}
	id, ok = o.idFromNFO(ctx, "/lib/剧", "剧", true, l.dirs["/lib/剧"], identify.Result{Kind: "tv"})
	if !ok || id != "tv/1399" {
		t.Fatalf("tvshow.nfo: %q %v", id, ok)
	}
	// an NFO for a loose file is read from the listing of its folder
	if _, ok = o.idFromNFO(ctx, "/lib/Film (2001).mkv", "Film (2001).mkv", false, nil, identify.Result{Kind: "movie"}); ok {
		t.Fatal("an imdb id TMDB does not know was taken")
	}
	// no NFO in the listing: nothing is read
	n := len(l.reads)
	if _, ok = o.idFromNFO(ctx, "/lib/空目录", "空目录", true, l.dirs["/lib/空目录"], identify.Result{Kind: "movie"}); ok || len(l.reads) != n {
		t.Fatal("read a file that is not there")
	}
}

func TestNFOIsNotReadWhereFilesCannotBeRead(t *testing.T) {
	// a Lister that cannot read (the remote one) never has an NFO taken
	o := &Orchestrator{Lister: listOnly{}, Providers: tmdbKnowing(t, "movie/1")}
	if _, ok := o.idFromNFO(context.Background(), "/lib/x", "x", true, []identify.Entry{{Name: "movie.nfo"}}, identify.Result{Kind: "movie"}); ok {
		t.Fatal("read an NFO through a lister that cannot read")
	}
}

type listOnly struct{}

func (listOnly) List(context.Context, string) ([]identify.Entry, error) { return nil, nil }
