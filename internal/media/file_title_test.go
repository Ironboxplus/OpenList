package media

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestScanKeepsTheFileTitleApartFromTheScrapedOne(t *testing.T) {
	r := identify.ClassifyWith("[VCB-Studio] Another [Ma10p_1080p]", true, nil, nil)
	it := &model.MediaItem{FileName: "[VCB-Studio] Another [Ma10p_1080p]", IsFolder: true}
	applyIdentify(it, r)
	if it.FileTitle != "Another" || it.ScrapedName != "Another" {
		t.Fatalf("file title %q scraped %q", it.FileTitle, it.ScrapedName)
	}
	// a scrape renames the work, the scan's title stays
	it.ScrapedName = "Another (2012)"
	if d, a := it.DisplayTitles(model.TitleSourceFile); d != "Another" || a != "Another (2012)" {
		t.Fatalf("(%q, %q)", d, a)
	}
}
