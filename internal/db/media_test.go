package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestListMediaItemsFilteredPagesAfterFilteringAcrossBatches(t *testing.T) {
	conf.Conf = conf.DefaultConfig(t.TempDir())
	d, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:db-media-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	Init(d)
	old := mediaItemsBatch
	mediaItemsBatch = 3
	t.Cleanup(func() { mediaItemsBatch = old })

	// 10 items; the even ones are rejected, so 5 remain, spread over 4 batches
	for i := 0; i < 10; i++ {
		it := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: "/m", FileName: fmt.Sprintf("%02d", i), ScrapedName: fmt.Sprintf("%02d", i)}
		it.VideoType = model.MediaKindMovie
		if err := SaveVideoItem(it, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	odd := func(it *model.MediaItem) bool { return it.FileName[len(it.FileName)-1]%2 == 1 }
	page := func(n int) ([]string, int64) {
		items, total, err := ListMediaItemsFiltered(MediaItemQuery{
			MediaType: model.MediaTypeVideo, Kinds: []string{model.MediaKindMovie}, OrderBy: "name", Page: n, PageSize: 2,
		}, odd)
		if err != nil {
			t.Fatal(err)
		}
		var ps []string
		for _, it := range items {
			ps = append(ps, MediaItemPath(&it))
		}
		return ps, total
	}
	want := [][]string{{"/m/01", "/m/03"}, {"/m/05", "/m/07"}, {"/m/09"}, nil}
	for n, w := range want {
		got, total := page(n + 1)
		if total != 5 || fmt.Sprint(got) != fmt.Sprint(w) {
			t.Fatalf("page %d: got %v total %d, want %v total 5", n+1, got, total, w)
		}
	}
}
