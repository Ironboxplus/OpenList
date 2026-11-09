package _115_open

import (
	"testing"

	sdk "github.com/OpenListTeam/115-sdk-go"
)

func TestToVideoPlayInfos_SortAndMap(t *testing.T) {
	urls := []sdk.VideoPlayURL{
		{URL: "http://example.com/720", Definition: 2, Desc: "720P"},
		{URL: "http://example.com/1080", Definition: 4, Desc: "1080P"},
		{URL: "http://example.com/360", Definition: 1, Desc: "360P"},
	}

	got := toVideoPlayInfos(urls)

	if len(got) != 3 {
		t.Fatalf("expected 3 infos, got %d", len(got))
	}

	// Sorted by Definition descending (highest quality first).
	wantDefinitions := []int{4, 2, 1}
	wantResolutions := []string{"1080P", "720P", "360P"}
	wantURLs := []string{"http://example.com/1080", "http://example.com/720", "http://example.com/360"}

	for i := range got {
		if got[i].Definition != wantDefinitions[i] {
			t.Errorf("index %d: Definition = %d, want %d", i, got[i].Definition, wantDefinitions[i])
		}
		if got[i].Resolution != wantResolutions[i] {
			t.Errorf("index %d: Resolution = %q, want %q", i, got[i].Resolution, wantResolutions[i])
		}
		if got[i].URL != wantURLs[i] {
			t.Errorf("index %d: URL = %q, want %q", i, got[i].URL, wantURLs[i])
		}
	}
}

func TestToVideoPlayInfos_Empty(t *testing.T) {
	got := toVideoPlayInfos(nil)
	if got == nil {
		t.Fatalf("expected non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %d elements", len(got))
	}
}

func TestToVideoSubtitleInfos_EmbeddedTitleAndOrder(t *testing.T) {
	// Shape of a real 115 answer for a file with twelve container subtitle
	// tracks: the first ten as "[内置字幕]" + the track's own title, in
	// container order, then a blank entry.
	titles := []string{"印尼语", "法语", "泰语", "简体中文", "繁体中文", "英语", "葡萄牙语", "西班牙语", "越南语", "阿拉伯语"}
	var list []sdk.SubtitleItem
	for _, title := range titles {
		list = append(list, sdk.SubtitleItem{
			Title:    "[内置字幕]" + title,
			Type:     "srt",
			URL:      "http://example.com/sub/" + title,
			Language: "x",
		})
	}
	list = append(list, sdk.SubtitleItem{URL: "http://example.com/sub/blank"})

	got := toVideoSubtitleInfos(list)

	if len(got) != len(titles) {
		t.Fatalf("expected %d infos (blank entry dropped), got %d", len(titles), len(got))
	}
	for i, title := range titles {
		if got[i].EmbeddedTitle != title {
			t.Errorf("index %d: EmbeddedTitle = %q, want %q", i, got[i].EmbeddedTitle, title)
		}
		if got[i].EmbeddedOrder == nil || *got[i].EmbeddedOrder != i {
			t.Errorf("index %d: EmbeddedOrder = %v, want %d", i, got[i].EmbeddedOrder, i)
		}
		if got[i].Title != "[内置字幕]"+title {
			t.Errorf("index %d: Title = %q, want the provider title unchanged", i, got[i].Title)
		}
	}
}

func TestToVideoSubtitleInfos_OnlyPrefixedEntriesAreEmbedded(t *testing.T) {
	list := []sdk.SubtitleItem{
		{Title: "movie.zh.srt", Type: "srt", URL: "http://example.com/a"},
		{Title: "[内置字幕]English", Type: "srt", URL: "http://example.com/b"},
	}

	got := toVideoSubtitleInfos(list)

	if len(got) != 2 {
		t.Fatalf("expected 2 infos, got %d", len(got))
	}
	if got[0].EmbeddedTitle != "" || got[0].EmbeddedOrder != nil {
		t.Errorf("sidecar entry must carry no embedded fields, got %q / %v", got[0].EmbeddedTitle, got[0].EmbeddedOrder)
	}
	// The order counts container tracks only, so a sidecar file before it
	// does not push it to 1.
	if got[1].EmbeddedOrder == nil || *got[1].EmbeddedOrder != 0 {
		t.Errorf("EmbeddedOrder = %v, want 0", got[1].EmbeddedOrder)
	}
}

func TestToVideoSubtitleInfos_DroppedTrackKeepsItsSlot(t *testing.T) {
	list := []sdk.SubtitleItem{
		{Title: "[内置字幕]A", Type: "srt", URL: "http://example.com/a"},
		{Title: "[内置字幕]B", Type: "srt", URL: ""},
		{Title: "[内置字幕]C", Type: "srt", URL: "http://example.com/c"},
	}

	got := toVideoSubtitleInfos(list)

	if len(got) != 2 {
		t.Fatalf("expected 2 infos, got %d", len(got))
	}
	if *got[1].EmbeddedOrder != 2 || got[1].EmbeddedTitle != "C" {
		t.Errorf("third container track must stay at order 2, got %v / %q", *got[1].EmbeddedOrder, got[1].EmbeddedTitle)
	}
}

func TestToVideoSubtitleInfos_FiltersEmptyAndCaptionMap(t *testing.T) {
	list := []sdk.SubtitleItem{
		{Title: "", Type: "", URL: "http://example.com/blank"},
		{Title: "no url", URL: ""},
		{Title: "map", URL: "http://example.com/map", IsCaptionMap: 1},
		{Title: "[内置字幕]", URL: "http://example.com/prefix-only"},
		{Title: "ok", Type: "srt", URL: "http://example.com/ok"},
	}

	got := toVideoSubtitleInfos(list)

	if len(got) != 2 {
		t.Fatalf("expected 2 infos, got %d: %+v", len(got), got)
	}
	if got[0].Title != "[内置字幕]" || got[0].EmbeddedOrder != nil {
		t.Errorf("a prefix with no title is kept as listed but is not an alias, got %+v", got[0])
	}
	if got[1].Title != "ok" {
		t.Errorf("Title = %q, want ok", got[1].Title)
	}
}
