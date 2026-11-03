package identify

import "testing"

func TestSeasonMarkerAnywhereInADirectoryName(t *testing.T) {
	for name, want := range map[string]int{
		"Friends.S01.1080p.BluRay.x265-RARBG":      1,
		"Jojo No Kimyou Na Bouken S06 Stone Ocean": 6,
		"葬送的芙莉莲 S01":                              1,
		"绝命D师 第3季 2008 豆瓣9.2":                     3,
		"【熟肉】Love.Death.and.Robots.S02":          2,
		"Love.Death.and.Robots.S03-v2":             3,
		"S01高码率":                                   1,
		"BEASTARS 2nd Season":                      2,
		"Season 4":                                 4,
		"第三季":                                      3,
		"1":                                        1,
		"02":                                       2,
	} {
		if !IsSeasonDir(name) {
			t.Errorf("%q should be a season directory", name)
		}
		if got := seasonOfDirName(name); got != want {
			t.Errorf("%q: season %d, want %d", name, got, want)
		}
	}
	for _, name := range []string{
		"Mo Dao Zu Shi III", "S.A.C. 2nd GIG", "STAND ALONE COMPLEX", "Se7en (1995)", "Pulp Fiction (1994)",
		"2012", "Ocean's 11", "Oppenheimer.2023.DTS5.1", "Friends.S01E01", "[Subs]", "Frieren", "Toy Story 3",
	} {
		if IsSeasonDir(name) {
			t.Errorf("%q should not be a season directory", name)
		}
	}
}

func TestSeasonDirsMakeOneShow(t *testing.T) {
	for _, c := range []struct {
		name    string
		entries []Entry
	}{
		{"老友记", dirs("Friends.S01.1080p.BluRay.x265-RARBG", "Friends.S02.1080p.BluRay.x265-RARBG", "Friends.S10.1080p.BluRay.x265-RARBG", "[Subs]")},
		{"绝命毒师", cat(dirs("绝命D师 第1季 2008", "绝命D师 第2季 2009", "绝命D师 第5季 2012"), dirs("绝命D师电影 续命之徒 2019"))},
		{"JOJO合集", cat(dirs("Jojo No Kimyou Na Bouken S01 Phantom Blood", "Jojo No Kimyou Na Bouken S02 Battle Tendency"), vids("JoJos.Bizarre.Adventure.S06E01.mkv"))},
		{"芙莉莲", dirs("葬送的芙莉莲 S01", "Frieren")},
		{"爱死机", dirs("Love.Death.&.Robots.S01.1080p", "S04 FLUX", "【熟肉】Love.Death.and.Robots.S02", "【熟肉】Love.Death.and.Robots.S03", "【熟肉】Love.Death.and.Robots.S03-v2")},
		{"时光代理人", dirs("1", "2", "英都篇")},
		{"动物狂想曲", dirs("BEASTARS", "BEASTARS 2nd Season")},
	} {
		if s := ClassifyEntries(c.entries); s.Kind != "tv" {
			t.Errorf("%s: kind %q (%s), want tv", c.name, s.Kind, s.Why)
		}
	}
}

// Season directories of different shows are a collection of shows.
func TestSeasonDirsOfDifferentShowsStayACollection(t *testing.T) {
	s := ClassifyEntries(dirs("Daredevil S01", "Jessica Jones S01", "Luke Cage S01"))
	if s.Kind != "collection" {
		t.Fatalf("kind %q (%s), want collection", s.Kind, s.Why)
	}
}

func TestEpisodeFilesInSeasonNamedDirs(t *testing.T) {
	tree := map[string][]Entry{
		"": cat(dirs("Friends.S01.1080p", "Friends.S02.1080p", "[Subs]")),
		"Friends.S01.1080p": vids("Friends.S01E01.mkv", "Friends.S01E02.mkv"),
		"Friends.S02.1080p": vids("Friends.S02E01.mkv"),
	}
	got, err := EpisodeFiles(func(rel string) ([]Entry, error) { return tree[rel], nil })
	if err != nil || len(got) != 3 {
		t.Fatalf("got %v err %v, want 3 episodes", got, err)
	}
}

// A film kept next to the seasons is a season-0 special (the Jellyfin
// convention), not an episode of a numbered season.
func TestFilmNextToSeasonsIsASpecial(t *testing.T) {
	tree := map[string][]Entry{
		"": dirs("绝命D师 第1季", "绝命D师 第2季", "绝命D师电影 续命之徒 2019"),
		"绝命D师 第1季":        eps("Breaking.Bad.S01E%02d.mkv", 2),
		"绝命D师 第2季":        eps("Breaking.Bad.S02E%02d.mkv", 2),
		"绝命D师电影 续命之徒 2019": one("El.Camino.2019.1080p.mkv"),
	}
	got, err := EpisodeFiles(func(rel string) ([]Entry, error) { return tree[rel], nil })
	if err != nil {
		t.Fatal(err)
	}
	var specials []EpisodeFile
	for _, f := range got {
		if f.Season == 0 {
			specials = append(specials, f)
		}
	}
	if len(got) != 5 || len(specials) != 1 || specials[0].Episode != 1 || specials[0].Name != "El.Camino.2019.1080p.mkv" {
		t.Fatalf("got %+v", got)
	}
}
