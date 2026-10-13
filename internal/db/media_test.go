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
	old := mediaFilterBatch
	mediaFilterBatch = 3
	t.Cleanup(func() { mediaFilterBatch = old })

	// 10 items; the even ones are rejected, so 5 remain, spread over 4 batches
	for i := 0; i < 10; i++ {
		it := &model.MediaItem{Path: fmt.Sprintf("/m/%02d", i), Kind: model.MediaKindMovie, Title: "t"}
		if err := SaveMediaItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	odd := func(it *model.MediaItem) bool { return it.Path[len(it.Path)-1]%2 == 1 }
	page := func(n int) ([]string, int64) {
		items, total, err := ListMediaItemsFiltered(MediaListFilter{Prefix: "/m", Kinds: []string{model.MediaKindMovie}, Page: n, PerPage: 2}, odd)
		if err != nil {
			t.Fatal(err)
		}
		var ps []string
		for _, it := range items {
			ps = append(ps, it.Path)
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
