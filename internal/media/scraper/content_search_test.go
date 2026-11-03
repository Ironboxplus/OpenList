package scraper

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestScoreMatchUsesTheOriginalTitleToo(t *testing.T) {
	// TMDB's zh-CN title of this film is 飞越疯人院; the files say the English name
	cuckoo := tmdbHit{ID: 510, Title: "飞越疯人院", OriginalTitle: "One Flew Over the Cuckoo's Nest", ReleaseDate: "1975-11-19", VoteAverage: 8.4}
	if s := scoreMatch("One Flew Over the Cuckoos Nest", "1975", cuckoo); s < StrongMatch {
		t.Errorf("cuckoo's nest scores %.2f", s)
	}
	// its zh-CN title is English: "Love Death & Robots" is the same title
	ldr := tmdbHit{ID: 86831, Name: "Love, Death & Robots", OriginalName: "Love, Death & Robots", FirstAirDate: "2019-03-15", VoteAverage: 8.2}
	if s := titleSimilarity("Love Death and Robots", ldr.Name); s < 0.8 {
		t.Errorf("& and 'and' differ: %.2f", s)
	}
}

// 回忆三部曲 searched as "回忆" finds 杀人回忆 first; the English name and year in
// the files ("Memories" 1995) find the right film, and the year settles it.
func TestIdentifyUsesTheNamesFoundInsideTheDirectory(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		switch {
		case strings.Contains(q, "回忆"):
			writeJSON(w, []byte(`{"results":[{"id":11423,"title":"杀人回忆","original_title":"살인의 추억","release_date":"2003-04-25","vote_average":8.1}]}`))
		case q == "Memories":
			writeJSON(w, []byte(`{"results":[{"id":42994,"title":"回忆三部曲","original_title":"MEMORIES","release_date":"1995-12-23","vote_average":7.5},{"id":1691866,"title":"Memories of Fear","original_title":"Memories of Fear","release_date":"1995-01-01","vote_average":2}]}`))
		default:
			writeJSON(w, []byte(`{"results":[]}`))
		}
	})
	cfg := fastConfig(map[string]string{"tmdb": srv.URL + "/3"})
	cfg.TMDBToken = fakeBearer
	set, _ := NewSet(cfg)
	q := IdentifyQuery{Terms: SearchTerms(ParseName("回忆")), Kind: "movie", Content: []ContentName{{Name: "Memories", Year: 1995}}}
	cs, err := set.TMDB.Identify(context.Background(), q)
	if err != nil || len(cs) == 0 || cs[0].ID != "movie/42994" || cs[0].Score < StrongMatch {
		t.Fatalf("candidates %+v (%v)", cs, err)
	}
	// without the content names the wrong film wins, as on the first rescan
	q.Content = nil
	cs, _ = set.TMDB.Identify(context.Background(), q)
	if len(cs) == 0 || cs[0].ID != "movie/11423" {
		t.Fatalf("without content: %+v", cs)
	}
}

// A clean match on title and year needs no second opinion.
func TestIdentifySkipsTheContentSearchWhenTheNameIsClean(t *testing.T) {
	n := 0
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		writeJSON(w, []byte(`{"results":[{"id":27205,"title":"盗梦空间","original_title":"Inception","release_date":"2010-07-15","vote_average":8.4}]}`))
	})
	cfg := fastConfig(map[string]string{"tmdb": srv.URL + "/3"})
	cfg.TMDBToken = fakeBearer
	set, _ := NewSet(cfg)
	cs, err := set.TMDB.Identify(context.Background(), IdentifyQuery{Terms: SearchTerms(ParseName("盗梦空间 (2010)")), Kind: "movie", Year: 2010,
		Content: []ContentName{{Name: "Inception", Year: 2010}}})
	if err != nil || len(cs) == 0 || cs[0].Score < 0.85 || n != 1 {
		t.Fatalf("%d requests, candidates %+v (%v)", n, cs, err)
	}
}

func TestAProviderRequestThatStalledIsTriedOnce(t *testing.T) {
	calls := 0
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			time.Sleep(300 * time.Millisecond) // longer than the client timeout below
		}
		writeJSON(w, []byte(`{"data":[]}`))
	})
	cfg := fastConfig(map[string]string{"bgm": srv.URL})
	cfg.Timeout = 100 * time.Millisecond
	set, _ := NewSet(cfg)
	_, err := set.BGM.api.do(context.Background(), func() (*http.Request, error) {
		return http.NewRequest("GET", srv.URL+"/x", nil)
	})
	if err != nil || calls != 2 {
		t.Fatalf("calls %d err %v, want the second try to succeed", calls, err)
	}
}

// A show names the years of its later seasons and of what sits beside it (食戟之灵
// holds a 2021 film): the year of a content name does not count against a show.
func TestContentYearDoesNotCountAgainstAShow(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if q == "食戟之灵" {
			writeJSON(w, []byte(`{"results":[{"id":62273,"name":"食戟之灵","original_name":"食戟のソーマ","first_air_date":"2015-04-06","vote_average":8.2}]}`))
			return
		}
		writeJSON(w, []byte(`{"results":[]}`))
	})
	cfg := fastConfig(map[string]string{"tmdb": srv.URL + "/3"})
	cfg.TMDBToken = fakeBearer
	set, _ := NewSet(cfg)
	q := IdentifyQuery{Terms: SearchTerms(ParseName("食戟之灵")), Kind: "tv", Content: []ContentName{{Name: "Shoujo Kageki Revue Starlight The Movie", Year: 2021}}}
	cs, err := set.TMDB.Identify(context.Background(), q)
	if err != nil || len(cs) == 0 || cs[0].ID != "tv/62273" || cs[0].Score < StrongMatch {
		t.Fatalf("candidates %+v (%v)", cs, err)
	}
}

// A year right before another year is part of the title.
func TestTheYearOfAReleaseIsTheLastOne(t *testing.T) {
	for name, want := range map[string][2]string{
		"The.Legend.of.1900.1998.1080p.BluRay.REMUX.VC-1.DTS-HD.MA.5.1": {"The Legend of 1900", "1998"},
		"Blade.Runner.2049.2017.2160p.WEB-DL":                           {"Blade Runner 2049", "2017"},
		"Inception.2010.1080p.BluRay":                                   {"Inception", "2010"},
	} {
		p := ParseName(name)
		if p.English != want[0] || p.Year != want[1] {
			t.Errorf("%s: %q %q, want %q %q", name, p.English, p.Year, want[0], want[1])
		}
	}
}
