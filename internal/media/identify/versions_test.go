package identify

import (
	"fmt"
	"reflect"
	"testing"
)

func TestVersionsOfOneFilm(t *testing.T) {
	s := ClassifyEntries([]Entry{
		{Name: "av1_recode_Flipped.2010.BluRay.1080p.x265.mkv", Size: 3 * gb},
		{Name: "Flipped.2010.BluRay.1080p.x265.mkv", Size: 7 * gb},
	})
	want := []string{"Flipped.2010.BluRay.1080p.x265.mkv", "av1_recode_Flipped.2010.BluRay.1080p.x265.mkv"}
	if s.Kind != "movie" || !reflect.DeepEqual(s.Versions, want) {
		t.Fatalf("kind %q versions %v (%s), want movie %v", s.Kind, s.Versions, s.Why, want)
	}

	// a cut, its re-encode and a file that only says REMUX
	s = ClassifyEntries([]Entry{
		{Name: "American.Psycho.2000.CEE.BluRay.1080p.x264.AC3-CMCT.mkv", Size: 8 * gb},
		{Name: "American.Psycho.2000.UNCUT.2160p.BluRay.REMUX.HEVC-FGT.mkv", Size: 10 * gb},
		{Name: "REMUX.mkv", Size: 60 * gb},
	})
	if s.Kind != "movie" || len(s.Versions) != 3 {
		t.Fatalf("kind %q versions %v (%s)", s.Kind, s.Versions, s.Why)
	}

	// built-in subtitles suffix
	s = ClassifyEntries([]Entry{
		{Name: "The.Silence.of.the.Lambs.1991.CC.Remastered-FRDS.mkv", Size: 12 * gb},
		{Name: "The.Silence.of.the.Lambs.1991.CC.Remastered-FRDS-sub_built_in.mkv", Size: 5 * gb},
	})
	if s.Kind != "movie" || len(s.Versions) != 2 {
		t.Fatalf("kind %q versions %v (%s)", s.Kind, s.Versions, s.Why)
	}

	// one original that dominates the bytes is still listed with its version
	s = ClassifyEntries([]Entry{
		{Name: "The.Teahouse.1982.1080p.REMUX.mkv", Size: 35 * gb},
		{Name: "av1_recode_The.Teahouse.1982.1080p.REMUX.mkv", Size: 5 * gb},
	})
	if s.Kind != "movie" || len(s.Versions) != 2 {
		t.Fatalf("kind %q versions %v (%s)", s.Kind, s.Versions, s.Why)
	}
}

// Films of a series are different works even when the names share a prefix.
func TestDifferentFilmsAreNotVersions(t *testing.T) {
	for name, entries := range map[string][]Entry{
		"deadpool":  {{Name: "AV1_Recode_Deadpool.2.2018.Super.Duper.Cut.mkv", Size: 5 * gb}, {Name: "AV1_Recode_Deadpool.2016.BluRay.mkv", Size: 4 * gb}},
		"infernal":  vids("[无间道II].Infernal.Affairs.II.2003.BluRay.mkv", "[无间道I].Infernal.Affairs.I.2002.BluRay.mkv", "[无间道III].Infernal.Affairs.III.2003.BluRay.mkv"),
		"killbill":  vids("Kill.Bill.Vol.1.2003.1080p.mkv", "Kill.Bill.Vol.2.2004.1080p.mkv"),
		"godfather": vids("The.Godfather.1972.1080p.mkv", "The.Godfather.Part.II.1974.1080p.mkv", "The.Godfather.Part.III.1990.1080p.mkv"),
	} {
		if s := ClassifyEntries(entries); s.Kind != "collection" || len(s.Versions) != 0 {
			t.Errorf("%s: kind %q versions %v (%s), want a collection", name, s.Kind, s.Versions, s.Why)
		}
	}
}

func TestSpecialsNextToALoneFilmAreItsExtras(t *testing.T) {
	s := ClassifyEntries(cat(dirs("特典"), []Entry{{Name: "5.Centimeters.Per.Second.2007.1080p.mkv", Size: 18 * gb}}))
	if s.Kind != "movie" {
		t.Fatalf("kind %q (%s), want movie", s.Kind, s.Why)
	}
	// ...and next to seasons they are what they say
	s = ClassifyEntries(dirs("Season 1", "Specials"))
	if s.Kind != "tv" {
		t.Fatalf("kind %q (%s), want tv", s.Kind, s.Why)
	}
}

func TestOrdinalPrefixedExtrasDoNotMakeANumberedSeries(t *testing.T) {
	tree := map[string][]Entry{"3. Movie": cat(one("Good.Will.Hunting.1997.REMUX.mkv"))}
	peek := func(n string) ([]Entry, error) { return tree[n], nil }
	s := ClassifyEntriesWith(dirs("3. Movie", "2. Pictures", "1. Sample"), peek)
	if s.Kind != "movie" {
		t.Fatalf("kind %q (%s), want movie", s.Kind, s.Why)
	}
}

func TestSingleSeasonBesideASequelTitleIsACollection(t *testing.T) {
	if s := ClassifyEntries(dirs("FLCL S01", "FLCL Alternative & Progressive ")); s.Kind != "collection" {
		t.Errorf("FLCL: kind %q (%s), want collection", s.Kind, s.Why)
	}
	// the same show under two names is still one show
	for name, d := range map[string][]Entry{
		"frieren":  dirs("葬送的芙莉莲 S01", "Frieren"),
		"beastars": dirs("[Nekomoe kissaten&VCB-Studio] BEASTARS [Ma10p_1080p]", "[Nekomoe kissaten&VCB-Studio] BEASTARS 2nd Season [Ma10p_1080p]"),
	} {
		if s := ClassifyEntries(d); s.Kind != "tv" {
			t.Errorf("%s: kind %q (%s), want tv", name, s.Kind, s.Why)
		}
	}
}

func TestNumberedDirectoriesAreFilmsOrSeasonsByWhatTheyHold(t *testing.T) {
	film := func(n int) []Entry { return one(fmt.Sprintf("Along.with.the.Gods.%d.2017.mkv", n)) }
	tree := map[string][]Entry{"1": film(1), "2": film(2)}
	peek := func(n string) ([]Entry, error) { return tree[n], nil }
	if s := ClassifyEntriesWith(dirs("1", "2"), peek); s.Kind != "collection" {
		t.Errorf("films: kind %q (%s), want collection", s.Kind, s.Why)
	}
	tree = map[string][]Entry{"1": eps("Link.Click.S01E%02d.mkv", 12), "2": eps("Link.Click.S02E%02d.mkv", 12)}
	if s := ClassifyEntriesWith(dirs("英都篇", "1", "2"), peek); s.Kind != "tv" {
		t.Errorf("seasons: kind %q (%s), want tv", s.Kind, s.Why)
	}
	// without a way to look, numbered directories stay seasons
	if s := ClassifyEntries(dirs("1", "2")); s.Kind != "tv" {
		t.Errorf("no peek: kind %q (%s), want tv", s.Kind, s.Why)
	}
	// 死神来了1..5: numbered, each one film
	tree = map[string][]Entry{}
	var names []string
	for i := 1; i <= 5; i++ {
		n := fmt.Sprintf("死神来了%d 1080P REMUX", i)
		names = append(names, n)
		tree[n] = film(i)
	}
	if s := ClassifyEntriesWith(dirs(names...), peek); s.Kind != "collection" {
		t.Errorf("死神来了: kind %q (%s), want collection", s.Kind, s.Why)
	}
}

func TestWrapperDirectoryTakesWhatItHolds(t *testing.T) {
	tree := map[string][]Entry{
		"Recode":     eps("AV1_Recode_Chernobyl.S01E%02d.mkv", 5),
		"main movie": one("Triangle.2009.mkv"),
	}
	peek := func(n string) ([]Entry, error) { return tree[n], nil }
	if s := ClassifyEntriesWith(dirs("Recode"), peek); s.Kind != "tv" {
		t.Errorf("Recode: kind %q (%s), want tv", s.Kind, s.Why)
	}
	if s := ClassifyEntriesWith(dirs("main movie", "sample video"), peek); s.Kind != "movie" {
		t.Errorf("main movie: kind %q (%s), want movie", s.Kind, s.Why)
	}
	if s := ClassifyEntries(dirs("Recode")); s.Kind != "" {
		t.Errorf("no peek: kind %q, want no verdict", s.Kind)
	}
}
