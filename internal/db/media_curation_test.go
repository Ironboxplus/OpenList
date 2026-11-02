package db

import (
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func newCurationDB(t *testing.T) {
	t.Helper()
	db = openTestDB(t, "x_")
	if err := db.AutoMigrate(&model.MediaItem{}); err != nil {
		t.Fatal(err)
	}
}

func scanned(name string) *model.MediaItem {
	return &model.MediaItem{
		MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: name,
		IsFolder: true, ScrapedName: name, VideoType: model.VideoTypeMovie,
	}
}

func TestRescanKeepsLockedCuration(t *testing.T) {
	newCurationDB(t)
	it := scanned("Show")
	if err := CreateOrUpdateMediaItem(it); err != nil {
		t.Fatal(err)
	}
	it.ScrapedName, it.VideoType, it.Cover, it.Plot = "My Show", model.VideoTypeAnime, "thumb:/imgs/a.jpg", "plot"
	it.MediaCuration = model.MediaCuration{Locked: true, Source: model.MediaSourceManual, Confidence: 1, ExternalIDs: `{"tmdb":"tv/1"}`, Backdrop: "thumb:/imgs/b.jpg"}
	if err := UpdateMediaItem(it); err != nil {
		t.Fatal(err)
	}

	// the scanner builds a fresh item with only what it knows
	if err := CreateOrUpdateMediaItem(scanned("Show")); err != nil {
		t.Fatal(err)
	}
	got, err := GetMediaItemByID(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScrapedName != "My Show" || got.VideoType != model.VideoTypeAnime || got.Cover != "thumb:/imgs/a.jpg" || got.Plot != "plot" {
		t.Errorf("scraped fields were overwritten: %+v", got)
	}
	if !got.Locked || got.Source != model.MediaSourceManual || got.Confidence != 1 || got.ExternalIDs != `{"tmdb":"tv/1"}` || got.Backdrop != "thumb:/imgs/b.jpg" {
		t.Errorf("curation fields were overwritten: %+v", got.MediaCuration)
	}
}

func TestRescanRefreshesUnscrapedRuleItem(t *testing.T) {
	newCurationDB(t)
	first := scanned("Film")
	first.MediaCuration = model.MediaCuration{Source: model.MediaSourceRule, Confidence: 0.4}
	if err := CreateOrUpdateMediaItem(first); err != nil {
		t.Fatal(err)
	}
	second := scanned("Film")
	second.VideoType = model.VideoTypeTV
	second.MediaCuration = model.MediaCuration{Source: model.MediaSourceRule, Confidence: 0.9}
	if err := CreateOrUpdateMediaItem(second); err != nil {
		t.Fatal(err)
	}
	got, _ := GetMediaItemByID(first.ID)
	if got.VideoType != model.VideoTypeTV || got.Confidence != 0.9 {
		t.Errorf("an unscraped, unlocked item must follow the new scan: %+v", got)
	}
}

func TestRescanKeepsScrapedItem(t *testing.T) {
	newCurationDB(t)
	it := scanned("Done")
	if err := CreateOrUpdateMediaItem(it); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	it.ScrapedAt, it.ScrapedName, it.MediaCuration = &now, "Done (2020)", model.MediaCuration{Source: model.MediaSourceScraper, Confidence: 0.95}
	if err := UpdateMediaItem(it); err != nil {
		t.Fatal(err)
	}
	if err := CreateOrUpdateMediaItem(scanned("Done")); err != nil {
		t.Fatal(err)
	}
	got, _ := GetMediaItemByID(it.ID)
	if got.ScrapedName != "Done (2020)" || got.Confidence != 0.95 || got.Source != model.MediaSourceScraper {
		t.Errorf("scraped item changed by a rescan: %+v", got)
	}
}

func TestClearScrapedDataSparesLocked(t *testing.T) {
	newCurationDB(t)
	a, b := scanned("A"), scanned("B")
	for _, it := range []*model.MediaItem{a, b} {
		if err := CreateOrUpdateMediaItem(it); err != nil {
			t.Fatal(err)
		}
	}
	a.Cover, a.MediaCuration = "c", model.MediaCuration{Locked: true, ExternalIDs: "x"}
	b.Cover, b.MediaCuration = "c", model.MediaCuration{ExternalIDs: "x", Source: model.MediaSourceScraper}
	for _, it := range []*model.MediaItem{a, b} {
		if err := UpdateMediaItem(it); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ClearMediaScrapedData(model.MediaTypeVideo); err != nil {
		t.Fatal(err)
	}
	ga, _ := GetMediaItemByID(a.ID)
	gb, _ := GetMediaItemByID(b.ID)
	if ga.Cover != "c" || ga.ExternalIDs != "x" {
		t.Errorf("locked item was cleared: %+v", ga)
	}
	if gb.Cover != "" || gb.ExternalIDs != "" || gb.Source != "" {
		t.Errorf("unlocked item was not cleared: %+v", gb)
	}
}

func TestMediaRatingsReplaceAndClear(t *testing.T) {
	newCurationDB(t)
	if err := db.AutoMigrate(&model.MediaRating{}, &model.MediaAnnotation{}); err != nil {
		t.Fatal(err)
	}
	a, b := scanned("A"), scanned("B")
	b.Locked = true
	for _, it := range []*model.MediaItem{a, b} {
		if err := CreateOrUpdateMediaItem(it); err != nil {
			t.Fatal(err)
		}
	}
	two := []model.MediaRating{{Source: "tmdb", Value: 8.3}, {Source: "rt", Text: "93%"}}
	if err := ReplaceMediaRatings(a.ID, two); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceMediaRatings(b.ID, []model.MediaRating{{Source: "imdb", Value: 7}}); err != nil {
		t.Fatal(err)
	}
	// replacing leaves exactly the new set
	if err := ReplaceMediaRatings(a.ID, []model.MediaRating{{Source: "douban", Value: 9.1}}); err != nil {
		t.Fatal(err)
	}
	got, err := ListMediaRatings(a.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[a.ID]) != 1 || got[a.ID][0].Source != "douban" || len(got[b.ID]) != 1 {
		t.Fatalf("ratings = %+v", got)
	}
	if _, err := ClearMediaScrapedData(model.MediaTypeVideo); err != nil {
		t.Fatal(err)
	}
	got, _ = ListMediaRatings(a.ID, b.ID)
	if len(got[a.ID]) != 0 || len(got[b.ID]) != 1 {
		t.Errorf("clear must drop the unlocked item's ratings only: %+v", got)
	}
	if err := DeleteMediaItem(b.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = ListMediaRatings(b.ID)
	if len(got[b.ID]) != 0 {
		t.Errorf("deleting an item must delete its ratings")
	}
}

func TestPrimaryRating(t *testing.T) {
	if got := model.PrimaryRating(map[string]float32{"imdb": 7, "douban": 9}); got != 9 {
		t.Errorf("douban outranks imdb, got %v", got)
	}
	if got := model.PrimaryRating(map[string]float32{"tmdb": 8, "douban": 9}); got != 8 {
		t.Errorf("tmdb is the main rating, got %v", got)
	}
	if got := model.PrimaryRating(nil); got != 0 {
		t.Errorf("got %v", got)
	}
}
