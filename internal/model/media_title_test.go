package model

import "testing"

func TestDisplayTitles(t *testing.T) {
	item := func(file, scraped string, fixed bool) *MediaItem {
		m := &MediaItem{FileName: "[Grp] " + file + " [1080p].mkv", FileTitle: file, ScrapedName: scraped}
		if fixed {
			m.Locked = true
		}
		return m
	}
	for _, c := range []struct {
		name         string
		it           *MediaItem
		src          string
		display, alt string
	}{
		{"file mode shows the cleaned name", item("Steins;Gate 0", "命运石之门0", false), TitleSourceFile, "Steins;Gate 0", "命运石之门0"},
		{"scraped mode shows the scraped title", item("Steins;Gate 0", "命运石之门0", false), TitleSourceScraped, "命运石之门0", "Steins;Gate 0"},
		{"same title: no alt", item("Inception", "Inception", false), TitleSourceFile, "Inception", ""},
		{"locked title stays as set (file)", item("Steins;Gate 0", "我的名字", true), TitleSourceFile, "我的名字", ""},
		{"locked title stays as set (scraped)", item("Steins;Gate 0", "我的名字", true), TitleSourceScraped, "我的名字", ""},
		{"picked by hand is fixed too", func() *MediaItem {
			m := item("A", "B", false)
			m.Source = MediaSourceManual
			return m
		}(), TitleSourceFile, "B", ""},
		{"no scan title: the file name without extension", &MediaItem{FileName: "Film.mkv", ScrapedName: "电影"}, TitleSourceFile, "Film", "电影"},
		{"folder name is kept whole", &MediaItem{FileName: "Show.2020", IsFolder: true, ScrapedName: "剧"}, TitleSourceFile, "Show.2020", "剧"},
		{"unknown source means scraped", item("A", "B", false), "", "B", "A"},
	} {
		d, a := c.it.DisplayTitles(c.src)
		if d != c.display || a != c.alt {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, d, a, c.display, c.alt)
		}
	}
	m := item("Beta", "Alpha", false)
	_ = m.BeforeSave(nil)
	if m.SortName != MediaSortName("Alpha") || m.SortNameFile != MediaSortName("Beta") {
		t.Errorf("sort keys: %q %q", m.SortName, m.SortNameFile)
	}
}
