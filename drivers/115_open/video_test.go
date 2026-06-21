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
