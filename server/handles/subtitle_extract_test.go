package handles

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/media/subextract"
)

func subOrder(n int) *int { return &n }

func TestMissingTracksFollowTheOrderThenTheTitleOrLanguage(t *testing.T) {
	tracks := []subextract.TrackInfo{
		{Order: 0, Language: "eng", Title: "English", Supported: true},
		{Order: 1, Language: "chi", Title: "", Supported: true},
		{Order: 2, Language: "kor", Title: "", Supported: true},
		{Order: 3, Language: "jpn", Title: "Styled", Supported: true},
		{Order: 4, Codec: "S_VOBSUB", Language: "eng", Supported: false},
	}
	provided := []driver.VideoSubtitleInfo{
		// the same track by order and title
		{EmbeddedOrder: subOrder(0), EmbeddedTitle: "english"},
		// no title either side: the language decides, "zh" being "chi"
		{EmbeddedOrder: subOrder(1), Language: "zh"},
		// the order matches but the language does not: another track
		{EmbeddedOrder: subOrder(2), Language: "eng"},
		// a file without an order is matched by its title
		{EmbeddedTitle: "Styled"},
	}
	got := missingTracks(tracks, provided)
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("missing tracks %v, want [2] (3 is covered by title, 4 is unsupported)", got)
	}
}

func TestMissingTracksWithoutAProviderAreAllTheSupportedOnes(t *testing.T) {
	tracks := []subextract.TrackInfo{
		{Order: 0, Supported: true}, {Order: 1}, {Order: 2, Supported: true},
	}
	got := missingTracks(tracks, nil)
	if len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("got %v", got)
	}
}
