package db

import (
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// Every reader of the media tables must leave out a row the scan pruned.
// Readers that go through gorm's default scope get this for free; the ones that
// join, count a child table, or read user data by item id had to be fixed, and
// everything is checked here.
func TestPrunedRowsAppearInNoReader(t *testing.T) {
	db = openTestDB(t, "x_")
	if err := db.AutoMigrate(&model.MediaItem{}, &model.MediaRating{}, &model.MediaEpisode{}, &model.MediaUserData{}, &model.MediaScanPath{}); err != nil {
		t.Fatal(err)
	}
	sp := &model.MediaScanPath{Name: "m", Path: "/lib", MediaType: model.MediaTypeVideo, TypeTag: "movie"}
	if err := CreateMediaScanPath(sp); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) *model.MediaItem {
		it := &model.MediaItem{MediaType: model.MediaTypeVideo, ScanPathID: sp.ID, FolderPath: "/lib", FileName: name,
			IsFolder: true, ScrapedName: name, VideoType: model.VideoTypeTV, Cover: "thumb:/c.jpg"}
		it.Source = model.MediaSourceRule
		if err := SaveScannedMediaItem(it, true); err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.MediaEpisode{ItemID: it.ID, FileName: name + ".mkv", Season: 1, Episode: 1}).Error; err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		for _, d := range []model.MediaUserData{{UserID: 1, ItemID: it.ID, IsFavorite: true}, {UserID: 1, ItemID: it.ID, EpisodeIndex: 1, PositionTicks: 5, LastPlayedAt: &now}} {
			d := d
			if err := SaveMediaUserData(&d); err != nil {
				t.Fatal(err)
			}
		}
		return it
	}
	keep, gone := mk("Keep"), mk("Gone")
	if n, err := PruneMediaItems(sp.ID, map[uint]bool{keep.ID: true}, nil); err != nil || n != 1 {
		t.Fatalf("pruned %d (%v)", n, err)
	}

	ids := func(items []model.MediaItem) []uint {
		var out []uint
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}
	only := func(what string, got []uint) {
		t.Helper()
		if len(got) != 1 || got[0] != keep.ID {
			t.Errorf("%s = %v, want only %d", what, got, keep.ID)
		}
	}
	items, _, err := ListMediaItems(MediaItemQuery{MediaType: model.MediaTypeVideo})
	check(t, err)
	only("ListMediaItems", ids(items))
	items, _, err = ListMediaItems(MediaItemQuery{MediaType: model.MediaTypeVideo, TypeTag: "movie"}) // the JOIN
	check(t, err)
	only("ListMediaItems with a type tag (join)", ids(items))
	items, _, err = ListMediaItemsFiltered(MediaItemQuery{MediaType: model.MediaTypeVideo, TypeTag: "movie", Keyword: "Kee"}, func(*model.MediaItem) bool { return true })
	check(t, err)
	only("ListMediaItemsFiltered with tag and keyword", ids(items))
	items, _, err = ListVideoItems(VideoListFilter{Prefix: "/lib"})
	check(t, err)
	only("ListVideoItems", ids(items))
	items, err = ListVideoItemsUnder("/lib")
	check(t, err)
	only("ListVideoItemsUnder", ids(items))
	items, err = ListVideoCoversUnder("/lib", 10)
	check(t, err)
	only("ListVideoCoversUnder (child_posters)", ids(items))
	items, err = ListAllMediaItems(model.MediaTypeVideo)
	check(t, err)
	only("ListAllMediaItems (export)", ids(items))
	items, err = ListMediaItemsByScanPath(sp.ID)
	check(t, err)
	only("ListMediaItemsByScanPath (export)", ids(items))
	items, err = ListAllValidMediaItemsForCheck(model.MediaTypeVideo)
	check(t, err)
	only("ListAllValidMediaItemsForCheck (delete_invalid)", ids(items))
	items, err = GetUnscrappedItems(model.MediaTypeVideo, 0)
	check(t, err)
	only("GetUnscrappedItems", ids(items))
	if it, err := GetVideoItemByPath("/lib/Gone"); err != nil || it != nil {
		t.Errorf("GetVideoItemByPath found the pruned work: %v %v", it, err)
	}
	if _, err := GetMediaItemByID(gone.ID); err == nil {
		t.Error("GetMediaItemByID found the pruned work")
	}

	st, err := GetMediaStats()
	check(t, err)
	if st.TV != 1 || st.Episodes != 1 || st.Unscraped != 1 {
		t.Errorf("stats %+v, want one show, one episode, one unscraped", st)
	}
	favs, err := ListMediaFavorites(1)
	check(t, err)
	if len(favs) != 1 || favs[0].ItemID != keep.ID {
		t.Errorf("favourites %+v", favs)
	}
	resume, err := ListMediaResume(1, 10)
	check(t, err)
	if len(resume) != 1 || resume[0].ItemID != keep.ID {
		t.Errorf("resume list %+v", resume)
	}

	// saving the pruned path again (a manual match) takes the old row back
	again := &model.MediaItem{MediaType: model.MediaTypeVideo, ScanPathID: sp.ID, FolderPath: "/lib", FileName: "Gone", IsFolder: true, VideoType: model.VideoTypeMovie}
	if err := SaveVideoItem(again, nil, nil); err != nil || again.ID != gone.ID {
		t.Fatalf("saving a pruned path: id %d (%v), want %d", again.ID, err, gone.ID)
	}
	if _, err := GetMediaItemByID(gone.ID); err != nil {
		t.Errorf("the saved work is not visible: %v", err)
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
