package nfo

import "testing"

func TestParse(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want Info
	}{
		"kodi movie": {`<?xml version="1.0" encoding="UTF-8"?>
<movie><title>盗梦空间</title><originaltitle>Inception</originaltitle><year>2010</year>
<uniqueid type="tmdb" default="true">27205</uniqueid><uniqueid type="imdb">tt1375666</uniqueid></movie>`,
			Info{Kind: "movie", TMDB: "27205", IMDb: "tt1375666", Title: "盗梦空间", Year: 2010}},
		"tvshow with tmdbid tag": {`<tvshow><title>Show</title><premiered>2015-03-01</premiered><tmdbid>1399</tmdbid><imdbid>tt0944947</imdbid></tvshow>`,
			Info{Kind: "tv", TMDB: "1399", IMDb: "tt0944947", Title: "Show", Year: 2015}},
		"legacy id":  {`<movie><id>tt0133093</id></movie>`, Info{Kind: "movie", IMDb: "tt0133093"}},
		"only a url": {"https://www.themoviedb.org/tv/1399-game-of-thrones\n", Info{Kind: "tv", TMDB: "1399"}},
		"imdb url":   {"https://www.imdb.com/title/tt0133093/", Info{IMDb: "tt0133093"}},
		"url after the xml": {"<movie><title>X</title></movie>\nhttps://www.themoviedb.org/movie/603",
			Info{Kind: "movie", TMDB: "603", Title: "X"}},
		"a bad tmdb tag is not an id": {"\xef\xbb\xbf<movie><uniqueid type=\"tmdb\">abc</uniqueid><uniqueid type=\"imdb\">tt1234567</uniqueid></movie>",
			Info{Kind: "movie", IMDb: "tt1234567"}},
		"broken xml keeps its link": {"<movie><title>X</title> https://www.imdb.com/title/tt1234567/ <", Info{IMDb: "tt1234567"}},
	} {
		got, err := Parse([]byte(tc.in))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got.Kind != tc.want.Kind || got.TMDB != tc.want.TMDB || got.IMDb != tc.want.IMDb || got.Year != tc.want.Year || got.Title != tc.want.Title {
			t.Errorf("%s: got %+v, want %+v", name, got, tc.want)
		}
	}
	if _, err := Parse([]byte("not xml at all")); err == nil {
		t.Error("a file with no ids and no xml should be an error")
	}
	if _, err := Parse(nil); err == nil {
		t.Error("an empty file should be an error")
	}
}

func TestCandidatesAndFind(t *testing.T) {
	if c := Candidates(false, "Fight Club (1999).mkv", "", ""); len(c) != 1 || c[0] != "Fight Club (1999).nfo" {
		t.Fatalf("%v", c)
	}
	c := Candidates(true, "剧", "e01.mkv", "tv")
	if c[0] != "tvshow.nfo" || c[len(c)-1] != "剧.nfo" {
		t.Fatalf("%v", c)
	}
	if got := Find([]string{"a.mkv", "TVSHOW.NFO"}, c); got != "TVSHOW.NFO" {
		t.Fatalf("found %q", got)
	}
	if Find([]string{"a.mkv"}, c) != "" {
		t.Fatal("found something that is not there")
	}
}
