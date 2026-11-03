package identify

import "testing"

func TestEpisodesOfFolderThatHoldReleasesNotSeasons(t *testing.T) {
	tree := map[string][]Entry{
		"":                    dirs("Grandmaster release", "Other release"),
		"Grandmaster release": eps("Show.S01E%02d.mkv", 15),
		"Other release":       eps("Show.S01E%02d.mkv", 12),
	}
	got, err := EpisodeFiles(func(rel string) ([]Entry, error) { return tree[rel], nil })
	if err != nil || len(got) != 27 {
		t.Fatalf("got %d files (%v), want the 15 of the larger release and the 12 copies as versions", len(got), err)
	}
	for _, f := range got {
		if f.Season != 1 {
			t.Fatalf("season %d", f.Season)
		}
	}
}

// 星蝶公主: a 中英 variant with season folders and a 纯英 variant wrapped twice
// holding the same seasons plus S04E22-37 that the first lacks. An episode in
// both is kept once and its other file is a version; the extra ones are
// episodes of season 4.
func TestVariantDuplicatesAreVersionsNotDropped(t *testing.T) {
	const w = "纯英/Star.S01-S04.1080p-Mixed"
	const s4 = w + "/Star.S04.1080p.AMZN-CtrlHD"
	const s1 = w + "/Star.S01.1080p-iT00NZ"
	tree := map[string][]Entry{
		"":       dirs("中英", "纯英"),
		"中英":     dirs("第一季", "第四季"),
		"中英/第一季": eps("星蝶公主.Star.S01E%02d.HR-HDTV.mkv", 10),
		"中英/第四季": eps("星蝶公主.Star.S04E%02d.HR-HDTV.mkv", 21),
		"纯英":     dirs("Star.S01-S04.1080p-Mixed"),
		w:        dirs("Star.S04.1080p.AMZN-CtrlHD", "Star.S01.1080p-iT00NZ"),
		s1:       eps("Star.S01E%02d.1080p.mkv", 10),
		s4:       eps("Star.S04E%02d.1080p.mkv", 37),
	}
	got, err := EpisodeFiles(func(rel string) ([]Entry, error) { return tree[rel], nil })
	if err != nil {
		t.Fatal(err)
	}
	primary, versions := map[[2]int]bool{}, 0
	for _, f := range got {
		k := [2]int{f.Season, f.Episode}
		if f.Version {
			versions++
			continue
		}
		if primary[k] {
			t.Errorf("S%dE%d twice as an episode", f.Season, f.Episode)
		}
		primary[k] = true
	}
	for _, f := range got {
		if f.Version && !primary[[2]int{f.Season, f.Episode}] {
			t.Errorf("version %s of S%dE%d has no episode", f.Rel, f.Season, f.Episode)
		}
	}
	// the 纯英 variant (47 files) is the show; the 31 files of 中英 are versions
	if len(primary) != 47 || versions != 31 {
		t.Fatalf("%d episodes, %d versions, want 47 and 31", len(primary), versions)
	}
	if !primary[[2]int{4, 22}] || !primary[[2]int{4, 37}] {
		t.Error("S04E22-37 are not listed as episodes of season 4")
	}
}
