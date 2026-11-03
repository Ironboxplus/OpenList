package identify

import (
	"slices"
	"strings"
	"testing"
)

// 星蝶公主 as recorded on 2026-11-03: two release folders, 中英 holding its
// seasons in folders of their own (one named only by a number), 纯英 wrapping its
// seasons once more. Round 2 read episodes only from videos directly inside a
// release folder, so the show had none.
func TestEpisodesOfReleaseFoldersThatHoldTheirSeasonsInFolders(t *testing.T) {
	lib := loadLibrary(t)
	show := lib["Anime"].C["星蝶公主"]
	if show == nil {
		t.Fatal("星蝶公主 is not in the recorded library")
	}
	node := func(rel string) *libNode {
		n := show
		for _, part := range strings.Split(rel, "/") {
			if rel == "" {
				return n
			}
			if n = n.C[part]; n == nil {
				return nil
			}
		}
		return n
	}
	got, err := EpisodeFiles(func(rel string) ([]Entry, error) {
		if n := node(rel); n != nil {
			return n.entries(), nil
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, e := range show.C["中英"].entries() {
		for _, f := range show.C["中英"].C[e.Name].entries() {
			if !f.IsDir && IsVideo(f.Name) {
				want++
			}
		}
	}
	if want == 0 || len(got) != want {
		t.Fatalf("%d episodes, want the %d videos of 中英", len(got), want)
	}
	seasons := map[int]int{}
	for _, f := range got {
		seasons[f.Season]++
		if !strings.HasPrefix(f.Rel, "中英/") {
			t.Errorf("episode from %q: the English-only release must not mix in", f.Rel)
		}
	}
	for s := 1; s <= 4; s++ {
		if seasons[s] == 0 {
			t.Errorf("season %d has no episodes: %v", s, seasons)
		}
	}
	if len(seasons) != 4 {
		t.Errorf("seasons %v, want exactly 1-4", seasons)
	}
}

// A second release only fills the episodes the first lacks, and only where its
// files name their season.
func TestSecondReleaseFillsMissingEpisodesOnly(t *testing.T) {
	tree := map[string][]Entry{
		"":         dirs("A", "B"),
		"A":        dirs("wrap"),
		"A/wrap":   cat(dirs("x"), eps("Show.S01E%02d.mkv", 3)),
		"A/wrap/x": eps("Show.S02E%02d.mkv", 2),
		"B":        cat(eps("Show.S01E%02d.mkv", 5), []Entry{{Name: "notes.mkv", Size: gb}}),
	}
	got, err := EpisodeFiles(func(rel string) ([]Entry, error) { return tree[rel], nil })
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	versions := 0
	for _, f := range got {
		keys = append(keys, f.Rel)
		if f.Version {
			versions++
		}
	}
	// A holds S01E01-03 and S02E01-02 (5 files), B holds S01E01-05 (5 files):
	// the ties go to the first by name; B adds S01E04 and S01E05 and keeps its
	// copies of S01E01-03 as versions
	if len(got) != 10 || versions != 3 {
		t.Fatalf("%d files, %d versions: %v", len(got), versions, keys)
	}
	if !slices.Contains(keys, "B/Show.S01E05.mkv") || !slices.Contains(keys, "A/wrap/x/Show.S02E02.mkv") {
		t.Fatalf("episodes: %v", keys)
	}
}

func TestRecodePrefixIsNotPartOfTheTitle(t *testing.T) {
	for _, c := range []struct {
		name  string
		title string
		year  int
	}{
		{"AV1_Recode_GantzO.2016.1080p.BluRay.REMUX.AVC.DTS-HD.MA.5.1(1).mkv", "GantzO", 2016},
		{"AV1_Recode_Infernal.Affairs.II.2003.UHD.BluRay.2160p.3Audio.TrueHD.Atmos.7.1.HDR.x265.10bit.mUHD-FRDS.mkv", "Infernal Affairs II", 2003},
		{"av1_recode_Flipped.2010.1080p.BluRay.mkv", "Flipped", 2010},
	} {
		ni := ParseName(c.name, true)
		if ni.Title != c.title || ni.Year != c.year {
			t.Errorf("%s: title %q year %d, want %q %d", c.name, ni.Title, ni.Year, c.title, c.year)
		}
	}
	// a title that merely contains the word is left alone
	if ni := ParseName("Recode.2019.mkv", true); !strings.Contains(ni.Title, "Recode") {
		t.Errorf("title %q", ni.Title)
	}
}

func TestMemberWithoutATitleTakesTheCollectionTitle(t *testing.T) {
	two := Classify("2", true, []Entry{
		{Name: "Along.with.the.Gods.The.Last.49.Days.2018.BluRay.1080p.DTS-HD.MA.5.1.x265.10bit-BeiTai.mkv", Size: 12 * gb},
		{Name: "Along.with.the.Gods.The.Last.49.Days.2018.BluRay.1080p.DTS-HD.MA.5.1.x265.10bit-BeiTai.srt", Size: 121286},
	})
	if !two.InheritParentTitle("2", false, "与神同行") {
		t.Fatal("2 did not take the title")
	}
	if two.Title != "与神同行 2" || two.Year != 2018 {
		t.Fatalf("title %q year %d", two.Title, two.Year)
	}
	var names []string
	for _, tm := range two.Terms() {
		names = append(names, tm.Name)
		if tm.Year != "2018" {
			t.Errorf("term %q carries year %q, want the film's 2018", tm.Name, tm.Year)
		}
	}
	if !slices.Contains(names, "与神同行 2") || !slices.Contains(names, "与神同行") {
		t.Errorf("terms %v: want the numbered title and the collection title", names)
	}
	if len(two.Content) == 0 || !strings.Contains(two.Content[0].Name, "Last 49 Days") {
		t.Errorf("content names %v", two.Content)
	}

	// release tags only: the collection title without a number
	tags := Result{Kind: "tv", Title: "中英"}
	if !tags.InheritParentTitle("中英", false, "星蝶公主") || tags.Title != "星蝶公主" {
		t.Errorf("tags: %+v", tags)
	}
	// a file named by a number
	f := Result{Kind: "movie"}
	if !f.InheritParentTitle("02.mkv", true, "Kill Bill") || f.Title != "Kill Bill 2" {
		t.Errorf("file: %q", f.Title)
	}

	// names that are titles stay
	for _, name := range []string{"1917", "300", "Kill Bill", "2012 (2009)", "纯爱"} {
		r := Result{Kind: "movie", Title: name}
		if r.InheritParentTitle(name, false, "某合集") || r.Title != name {
			t.Errorf("%q took the collection title", name)
		}
	}
	// a collection, or a collection with no title of its own, takes nothing
	c := Result{Kind: "collection", Title: "1"}
	if c.InheritParentTitle("1", false, "某合集") {
		t.Error("a collection took a title")
	}
	m := Result{Kind: "movie", Title: "1"}
	if m.InheritParentTitle("1", false, "2") {
		t.Error("took the title of a parent that has none")
	}
}

func TestMaybeShow(t *testing.T) {
	base := Result{Kind: "collection", FromContent: true, ChildKinds: []string{"tv", ""}}
	if !base.MaybeShow() {
		t.Error("a content collection of non-films may be a show")
	}
	for name, mod := range map[string]func(*Result){
		"films":     func(r *Result) { r.ChildKinds = []string{"movie", "tv"} },
		"firm":      func(r *Result) { r.Firm = true },
		"unseen":    func(r *Result) { r.ChildKinds = nil },
		"by name":   func(r *Result) { r.FromContent = false },
		"not a set": func(r *Result) { r.Kind = "movie" },
	} {
		r := base
		mod(&r)
		if r.MaybeShow() {
			t.Errorf("%s: still maybe a show", name)
		}
	}
}

// The year of a content name is not the first number of a resolution, and a title
// that holds a year (The Legend of 1900) takes the release year after it.
func TestContentNameYears(t *testing.T) {
	c, ok := contentName("The.Legend.of.1900.1998.1080p.BluRay.REMUX.mkv", true)
	if !ok || c.Name != "The Legend of 1900" || c.Year != 1998 {
		t.Errorf("%+v %v", c, ok)
	}
	c, ok = contentName("[Shoujo Kageki Revue Starlight The Movie][BDRIP][1920x804][H264_FLAC].mkv", true)
	if !ok || c.Year != 0 {
		t.Errorf("a resolution was read as a year: %+v", c)
	}
}

func TestContentNameDropsABareEpisodeMarker(t *testing.T) {
	c, ok := contentName("Paranoia.Agent.E07.2004.1080p.BluRay.x264-WiKi.mkv", true)
	if !ok || c.Name != "Paranoia Agent" || c.Year != 2004 {
		t.Errorf("%+v %v", c, ok)
	}
}
