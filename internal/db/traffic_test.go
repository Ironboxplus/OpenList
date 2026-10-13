package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func testDB(t *testing.T) {
	t.Helper()
	conf.Conf = conf.DefaultConfig(t.TempDir())
	d, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:db-traffic-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "x_"}}) // as in production
	if err != nil {
		t.Fatal(err)
	}
	Init(d)
}

func TestAddTrafficDailyUpserts(t *testing.T) {
	testDB(t)
	for _, n := range []int64{100, 250} {
		if err := AddTrafficDaily("2026-10-13", "server_download", n); err != nil {
			t.Fatal(err)
		}
	}
	if err := AddTrafficDaily("2026-10-13", "server_upload", 7); err != nil {
		t.Fatal(err)
	}
	if err := AddTrafficDaily("2026-10-14", "server_download", 1); err != nil {
		t.Fatal(err)
	}
	rows, err := TrafficDailySince("2026-10-13")
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %v err = %v", rows, err)
	}
	sums, err := TrafficSums("", "")
	if err != nil || sums["server_download"] != 351 || sums["server_upload"] != 7 {
		t.Fatalf("sums = %v err = %v", sums, err)
	}
	one, _ := TrafficSums("2026-10-13", "2026-10-13")
	if one["server_download"] != 350 {
		t.Fatalf("one day = %v", one)
	}
}

func TestMediaStatsCountsWorksOnly(t *testing.T) {
	testDB(t)
	scraped := time.Now()
	items := []model.MediaItem{
		{Path: "/a", Kind: model.MediaKindMovie, ScrapedAt: &scraped},
		{Path: "/b", Kind: model.MediaKindMovie},
		{Path: "/c", Kind: model.MediaKindTV, ScrapedAt: &scraped},
		{Path: "/d", Kind: model.MediaKindAnime},
		{Path: "/e", Kind: model.MediaKindCollection, ScrapedAt: &scraped},
		{Path: "/f", Kind: model.MediaKindIgnore},
		{Path: "/g", Kind: model.MediaKindLibrary},
	}
	for i := range items {
		var eps []model.MediaEpisode
		if items[i].Kind == model.MediaKindTV {
			eps = []model.MediaEpisode{{FileName: "1.mkv", Season: 1, Episode: 1}, {FileName: "2.mkv", Season: 1, Episode: 2}}
		}
		if err := SaveMediaItem(&items[i], eps); err != nil {
			t.Fatal(err)
		}
	}
	got, err := GetMediaStats()
	if err != nil {
		t.Fatal(err)
	}
	want := MediaStats{Movie: 2, TV: 1, Anime: 1, Collection: 1, Episodes: 2, Unscraped: 2}
	if got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
}
