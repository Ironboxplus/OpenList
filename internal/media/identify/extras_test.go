package identify

import (
	"strings"
	"testing"
)

func TestExtraDirNamesWithBracketsAndFillerWords(t *testing.T) {
	for _, n := range []string{"[Subs]", "(Subs)", "【字幕】", "Subs", "sample video", "Sample.Video", "Samples", "Extras", "[Featurettes]", "trailers", "Behind the Scenes"} {
		if !IsExtraDir(n) {
			t.Errorf("%q should be an extras directory", n)
		}
	}
	for _, n := range []string{"Samples of Love", "Subspace", "main movie", "Friends.S01.1080p", "Extras and Evil", "[ANE] Kara no Kyoukai"} {
		if IsExtraDir(n) {
			t.Errorf("%q should not be an extras directory", n)
		}
	}
}

func TestMovieWithMainMovieAndSampleDirectoriesIsAMovie(t *testing.T) {
	r := Classify("恐怖游轮", true, dirs("main movie", "sample video"))
	if r.Kind != "movie" {
		t.Fatalf("kind %q (%s), want movie", r.Kind, r.Why)
	}
}

func TestSampleAndTrailerFilesDoNotMakeACollection(t *testing.T) {
	entries := []Entry{
		{Name: "Movie.2010.1080p.mkv", Size: 3 * gb},
		{Name: "Movie.2010.1080p.sample.mkv", Size: 1 * gb},
		{Name: "Movie.2010-trailer.mp4", Size: gb},
	}
	if s := ClassifyEntries(entries); s.Kind != "movie" {
		t.Fatalf("kind %q (%s), want movie", s.Kind, s.Why)
	}
}

func TestTinyVideosAreIgnoredWhenGrouping(t *testing.T) {
	v := []Entry{{Name: "a.mkv", Size: 4 * gb}, {Name: "b.mkv", Size: 20 << 20}, {Name: "c.mkv", Size: 150 << 20}}
	if got := mainVideos(v); len(got) != 1 || got[0].Name != "a.mkv" {
		t.Fatalf("mainVideos = %v", got)
	}
	// a season pack of small files stays whole
	if got := mainVideos(eps("x.S01E%02d.mkv", 5)); len(got) != 5 {
		t.Fatalf("kept %d of 5 episodes", len(got))
	}
}

// A title that is a year ("1917") must not reach the search as a year: the
// provider then finds the 1917 films of that year instead of the film 1917.
func TestBareYearTitleIsNotSearchedAsYear(t *testing.T) {
	for _, name := range []string{"1917", "1917.mkv"} {
		isFile := strings.HasSuffix(name, ".mkv")
		ni := ParseName(name, isFile)
		if ni.Title != "1917" || ni.Year != 0 || ni.Parsed.Year != "" {
			t.Errorf("%q: title %q year %d parsed year %q", name, ni.Title, ni.Year, ni.Parsed.Year)
		}
		for _, term := range Classify(name, !isFile, one("x.mkv")).Terms() {
			if term.Year != "" {
				t.Errorf("%q: search term %q carries year %q", name, term.Name, term.Year)
			}
		}
	}
}
