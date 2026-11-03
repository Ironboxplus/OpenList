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

// With a table prefix the media_items table is not literally named
// "media_items"; every query, including the scan path JOIN and the id
// tiebreaker of the ordering, has to use the real names.
func TestListMediaItemsHonorsTablePrefix(t *testing.T) {
	for _, prefix := range []string{"x_", "ol_", ""} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			db = openTestDB(t, prefix)
			if err := db.AutoMigrate(&model.MediaScanPath{}, &model.MediaItem{}); err != nil {
				t.Fatal(err)
			}
			sp := &model.MediaScanPath{MediaType: model.MediaTypeVideo, Name: "films", Path: "/films", TypeTag: "movie", ContentTags: "comedy,thriller"}
			if err := db.Create(sp).Error; err != nil {
				t.Fatal(err)
			}
			other := &model.MediaScanPath{MediaType: model.MediaTypeVideo, Name: "other", Path: "/other", TypeTag: "tv"}
			if err := db.Create(other).Error; err != nil {
				t.Fatal(err)
			}
			for i, name := range []string{"a", "b", "c"} {
				it := &model.MediaItem{MediaType: model.MediaTypeVideo, ScanPathID: sp.ID, FolderPath: "/films/x", FileName: name, ScrapedName: name, FileSize: int64(i)}
				it.VideoType = model.MediaKindMovie
				if err := db.Create(it).Error; err != nil {
					t.Fatal(err)
				}
			}
			it := &model.MediaItem{MediaType: model.MediaTypeVideo, ScanPathID: other.ID, FolderPath: "/other", FileName: "d", ScrapedName: "d"}
			it.VideoType = model.MediaKindTV
			if err := db.Create(it).Error; err != nil {
				t.Fatal(err)
			}

			cases := []struct {
				name string
				q    MediaItemQuery
				want int64
			}{
				{"plain", MediaItemQuery{MediaType: model.MediaTypeVideo}, 4},
				{"type tag", MediaItemQuery{MediaType: model.MediaTypeVideo, TypeTag: "movie"}, 3},
				{"content tag", MediaItemQuery{MediaType: model.MediaTypeVideo, ContentTag: "thriller"}, 3},
				{"both tags", MediaItemQuery{MediaType: model.MediaTypeVideo, TypeTag: "movie", ContentTag: "comedy"}, 3},
				{"path prefix", MediaItemQuery{MediaType: model.MediaTypeVideo, PathPrefix: "/films"}, 3},
				{"keyword", MediaItemQuery{MediaType: model.MediaTypeVideo, Keyword: "b"}, 1},
				{"kinds", MediaItemQuery{MediaType: model.MediaTypeVideo, Kinds: []string{model.MediaKindTV}}, 1},
				{"scan path", MediaItemQuery{MediaType: model.MediaTypeVideo, ScanPathID: sp.ID}, 3},
			}
			for _, c := range cases {
				for _, order := range []string{"", "name", "date", "size"} {
					q := c.q
					q.OrderBy, q.OrderDir = order, "desc"
					items, total, err := ListMediaItems(q)
					if err != nil {
						t.Fatalf("%s order %q: %v", c.name, order, err)
					}
					if total != c.want || int64(len(items)) != c.want {
						t.Fatalf("%s order %q: total %d len %d, want %d", c.name, order, total, len(items), c.want)
					}
				}
			}
		})
	}
}

func TestMediaNameOrderUsesTheSortKeyAndTopLevelHidesMembers(t *testing.T) {
	conf.Conf = conf.DefaultConfig(t.TempDir())
	d, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:db-media-sort-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	Init(d)
	save := func(folder, name, title string, isFolder bool) {
		it := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: folder, FileName: name, ScrapedName: title, IsFolder: isFolder}
		it.VideoType = model.MediaKindMovie
		if err := SaveVideoItem(it, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	save("/lib", "z", "周星驰", false)
	save("/lib", "a", "阿凡达", false)
	save("/lib", "m2", "Movie 2", false)
	save("/lib", "m10", "Movie 10", false)
	save("/lib", "coll", "合集", true)
	save("/lib/coll", "inner", "成员", false)

	names := func(q MediaItemQuery) []string {
		q.MediaType, q.OrderBy, q.PageSize = model.MediaTypeVideo, "name", 50
		items, _, err := ListMediaItems(q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.ScrapedName)
		}
		return out
	}
	want := []string{"阿凡达", "成员", "合集", "Movie 2", "Movie 10", "周星驰"}
	if got := names(MediaItemQuery{}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("flat: %v, want %v", got, want)
	}
	top := []string{"阿凡达", "合集", "Movie 2", "Movie 10", "周星驰"}
	if got := names(MediaItemQuery{TopLevel: true}); fmt.Sprint(got) != fmt.Sprint(top) {
		t.Fatalf("top level: %v, want %v", got, top)
	}
}

func TestMediaNameOrderFollowsTheTitleSource(t *testing.T) {
	conf.Conf = conf.DefaultConfig(t.TempDir())
	d, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:db-media-title-src-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	Init(d)
	save := func(name, file, scraped string, locked bool) {
		it := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: name, FileTitle: file, ScrapedName: scraped, IsFolder: true}
		it.VideoType, it.Locked = model.MediaKindMovie, locked
		if err := SaveVideoItem(it, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	// the file says Alpha, the scraper Zulu; and the reverse; and one a person fixed
	save("a", "Alpha", "Zulu", false)
	save("b", "Yankee", "Bravo", false)
	save("c", "Delta", "Mike", true)
	order := func(src string) string {
		items, _, err := ListMediaItems(MediaItemQuery{MediaType: model.MediaTypeVideo, OrderBy: "name", TitleSource: src, PageSize: 10})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.FileName)
		}
		return fmt.Sprint(out)
	}
	// file: Alpha, Mike (locked: as set), Yankee; scraped: Bravo, Mike, Zulu
	if got := order(model.TitleSourceFile); got != "[a c b]" {
		t.Errorf("file order %s", got)
	}
	if got := order(model.TitleSourceScraped); got != "[b c a]" {
		t.Errorf("scraped order %s", got)
	}

	// rows from before the columns existed get both keys and the file title at start-up
	old := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: "Old.Film.2001.1080p.mkv", ScrapedName: "Old"}
	old.VideoType = model.MediaKindMovie
	if err := SaveVideoItem(old, nil, nil); err != nil {
		t.Fatal(err)
	}
	db.Model(&model.MediaItem{}).Where("id = ?", old.ID).UpdateColumns(map[string]any{"sort_name": "", "sort_name_file": "", "file_title": ""})
	FileTitleOf = func(name string, _ bool) string { return "Old Film" }
	defer func() { FileTitleOf = func(string, bool) string { return "" } }()
	if err := backfillMediaFileTitles(); err != nil {
		t.Fatal(err)
	}
	if err := backfillMediaSortNames(); err != nil {
		t.Fatal(err)
	}
	var got model.MediaItem
	db.First(&got, old.ID)
	if got.FileTitle != "Old Film" || got.SortName != "old" || got.SortNameFile != "old film" {
		t.Errorf("backfill: %+v", got)
	}
}
