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
