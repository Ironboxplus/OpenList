package handles

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestViewCarriesDisplayAndAltTitle(t *testing.T) {
	it := &model.MediaItem{MediaType: model.MediaTypeVideo, FileName: "Another", IsFolder: true, FileTitle: "Another", ScrapedName: "Another 另一部"}
	it.VideoType = model.MediaKindTV
	for src, want := range map[string][2]string{
		model.TitleSourceFile:    {"Another", "Another 另一部"},
		model.TitleSourceScraped: {"Another 另一部", "Another"},
	} {
		v := &mediaViewer{rel: identityPath, titles: src}
		got := v.view(it)
		if got.DisplayTitle != want[0] || got.AltTitle != want[1] || got.ScrapedName != "Another 另一部" {
			t.Errorf("%s: display %q alt %q scraped_name %q", src, got.DisplayTitle, got.AltTitle, got.ScrapedName)
		}
	}
}
