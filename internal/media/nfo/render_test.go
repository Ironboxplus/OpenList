package nfo

import (
	"strings"
	"testing"
)

func TestRenderReadsBackAndEscapes(t *testing.T) {
	for _, in := range []Info{
		{Kind: "movie", TMDB: "27205", IMDb: "tt1375666", Title: "Tom & <Jerry>", OriginalTitle: "Inception", Year: 2010},
		{Kind: "tv", TMDB: "1399", Title: "Game of Thrones", Year: 2011},
		{Kind: "tv", IMDb: "tt0944947"},
	} {
		data, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		out, err := Parse(data)
		if err != nil || out != in {
			t.Errorf("round trip:\n%s\nin  %+v\nout %+v %v", data, in, out, err)
		}
	}
	data, _ := Render(Info{Kind: "tv", TMDB: "1"})
	if !strings.Contains(string(data), "<tvshow>") || !strings.Contains(string(data), `<uniqueid type="tmdb" default="true">1</uniqueid>`) {
		t.Errorf("shape:\n%s", data)
	}
	if got := FileName(false, "movie", "A (2001).mkv"); got != "A (2001).nfo" {
		t.Errorf("FileName: %q", got)
	}
}

func TestRenderDetailMovie(t *testing.T) {
	data, err := RenderDetail(Detail{
		Info:      Info{Kind: "movie", TMDB: "27205", IMDb: "tt1375666", Title: "Inception", OriginalTitle: "Inception", Year: 2010},
		Plot:      "A thief & his team <dream>.",
		Genres:    []string{"Action", "Sci-Fi"},
		Rating:    8.4,
		Premiered: "2010-07-16",
		Runtime:   148,
		Actors:    []Actor{{Name: "Leonardo DiCaprio", Role: "Cobb"}, {Name: "Elliot Page"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		"<movie>", "<plot>A thief &amp; his team &lt;dream&gt;.</plot>",
		"<genre>Action</genre>", "<genre>Sci-Fi</genre>", "<rating>8.4</rating>",
		"<premiered>2010-07-16</premiered>", "<releasedate>2010-07-16</releasedate>", "<runtime>148</runtime>",
		"<actor>\n    <name>Leonardo DiCaprio</name>\n    <role>Cobb</role>\n  </actor>",
		"<actor>\n    <name>Elliot Page</name>\n  </actor>",
		`<uniqueid type="tmdb" default="true">27205</uniqueid>`, `<uniqueid type="imdb">tt1375666</uniqueid>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	// the ids and the title still read back
	in, err := Parse(data)
	if err != nil || in.TMDB != "27205" || in.IMDb != "tt1375666" || in.Title != "Inception" || in.Year != 2010 || in.Kind != "movie" {
		t.Errorf("parse: %+v %v", in, err)
	}
}

func TestRenderDetailShowSeasonEpisode(t *testing.T) {
	show, _ := RenderDetail(Detail{Info: Info{Kind: "tv", TMDB: "1399", Title: "GoT"}, Plot: "p", Premiered: "2011-04-17"})
	if s := string(show); !strings.Contains(s, "<tvshow>") || !strings.Contains(s, "<premiered>2011-04-17</premiered>") || strings.Contains(s, "releasedate") {
		t.Errorf("tvshow:\n%s", s)
	}
	season, _ := RenderDetail(Detail{Root: RootSeason, Info: Info{Title: "Season 2"}, Season: 2})
	if s := string(season); !strings.Contains(s, "<season>") || !strings.Contains(s, "<seasonnumber>2</seasonnumber>") {
		t.Errorf("season:\n%s", s)
	}
	ep, _ := RenderDetail(Detail{Root: RootEpisode, Info: Info{Title: "Pilot"}, ShowTitle: "GoT", Plot: "o", Season: 1, Episode: 3})
	for _, want := range []string{"<episodedetails>", "<title>Pilot</title>", "<showtitle>GoT</showtitle>", "<season>1</season>", "<episode>3</episode>", "<plot>o</plot>"} {
		if !strings.Contains(string(ep), want) {
			t.Errorf("episode missing %q:\n%s", want, ep)
		}
	}
	if strings.Contains(string(ep), "seasonnumber") || strings.Contains(string(ep), "uniqueid") {
		t.Errorf("episode:\n%s", ep)
	}
	// a movie or a bare Info keeps the plain output
	plain, _ := Render(Info{Kind: "movie", TMDB: "1"})
	if strings.Contains(string(plain), "season") || strings.Contains(string(plain), "genre") {
		t.Errorf("plain:\n%s", plain)
	}
}
