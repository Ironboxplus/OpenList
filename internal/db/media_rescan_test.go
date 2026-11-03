package db

import (
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func rescanDB(t *testing.T) {
	t.Helper()
	db = openTestDB(t, "x_")
	if err := db.AutoMigrate(&model.MediaItem{}, &model.MediaRating{}, &model.MediaEpisode{}, &model.MediaUserData{}); err != nil {
		t.Fatal(err)
	}
}

// row saves a scanned row of scan path 7; kind is its video type.
func row(t *testing.T, folder, name, kind string) *model.MediaItem {
	t.Helper()
	it := &model.MediaItem{MediaType: model.MediaTypeVideo, ScanPathID: 7, FolderPath: folder, FileName: name,
		IsFolder: true, ScrapedName: name, VideoType: kind}
	it.Source, it.Confidence = model.MediaSourceRule, 0.7
	if err := SaveScannedMediaItem(it, true); err != nil {
		t.Fatal(err)
	}
	return it
}

func alive(t *testing.T, id uint) bool {
	t.Helper()
	_, err := GetMediaItemByID(id)
	return err == nil
}

// A collection of two files, rescanned as one film with versions: the parent
// keeps its id and its progress, the member rows are gone from every reader,
// and a member that comes back is restored with its id.
func TestRescanPrunesMembersOfAWorkThatBecameOneFilm(t *testing.T) {
	rescanDB(t)
	parent := row(t, "/lib", "怦然心动", model.VideoTypeCollection)
	m1 := row(t, "/lib/怦然心动", "Flipped.2010.mkv", model.VideoTypeMovie)
	m2 := row(t, "/lib/怦然心动", "av1_recode_Flipped.2010.mkv", model.VideoTypeMovie)
	other := row(t, "/lib", "Other", model.VideoTypeMovie)
	m1.Cover = "thumb:/a.jpg"
	if err := UpdateMediaItem(m1); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := SaveMediaUserData(&model.MediaUserData{UserID: 1, ItemID: parent.ID, PositionTicks: 99, LastPlayedAt: &now}); err != nil {
		t.Fatal(err)
	}

	// the new rules: the folder is one film; its members are not produced
	scanParent := row(t, "/lib", "怦然心动", model.VideoTypeMovie)
	scanOther := row(t, "/lib", "Other", model.VideoTypeMovie)
	if scanParent.ID != parent.ID {
		t.Fatalf("parent id changed: %d -> %d", parent.ID, scanParent.ID)
	}
	n, err := PruneMediaItems(7, map[uint]bool{scanParent.ID: true, scanOther.ID: true}, nil)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d (%v), want 2", n, err)
	}
	if alive(t, m1.ID) || alive(t, m2.ID) || !alive(t, parent.ID) || !alive(t, other.ID) {
		t.Fatal("wrong rows survived")
	}
	if d, err := GetMediaUserData(1, parent.ID, 0); err != nil || d.PositionTicks != 99 {
		t.Fatalf("progress of the parent lost: %+v %v", d, err)
	}

	// readers: the pruned member is not in the parent's members or child posters
	under, err := ListVideoItemsUnder("/lib/怦然心动")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range under {
		if it.ID == m1.ID || it.ID == m2.ID {
			t.Errorf("pruned member %s still listed under its parent", it.FileName)
		}
	}
	covers, err := ListVideoCoversUnder("/lib/怦然心动", 10)
	if err != nil || len(covers) != 0 {
		t.Fatalf("child posters of the parent: %v %v", covers, err)
	}

	// the member returns: same id, visible again
	back := row(t, "/lib/怦然心动", "Flipped.2010.mkv", model.VideoTypeMovie)
	if back.ID != m1.ID || !alive(t, m1.ID) {
		t.Fatalf("returning member got id %d, want %d", back.ID, m1.ID)
	}
}

func TestPruneNeverTouchesWhatItCannotJudge(t *testing.T) {
	rescanDB(t)
	kept := row(t, "/lib", "Kept", model.VideoTypeMovie)
	under := row(t, "/lib/Down", "Under.mkv", model.VideoTypeMovie)
	sibling := row(t, "/lib", "Sibling", model.VideoTypeMovie)
	locked := row(t, "/lib", "Locked", model.VideoTypeMovie)
	locked.Locked = true
	if err := UpdateMediaItem(locked); err != nil {
		t.Fatal(err)
	}
	manual := row(t, "/lib", "Manual", model.VideoTypeMovie)
	manual.Source = model.MediaSourceManual
	if err := UpdateMediaItem(manual); err != nil {
		t.Fatal(err)
	}
	foreign := row(t, "/elsewhere", "Foreign", model.VideoTypeMovie)
	foreign.ScanPathID = 8
	if err := UpdateMediaItem(foreign); err != nil {
		t.Fatal(err)
	}

	// /lib/Down failed to list: its row stays; the sibling, absent from the scan, goes
	n, err := PruneMediaItems(7, map[uint]bool{kept.ID: true}, []string{"/lib/Down"})
	if err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want 1", n, err)
	}
	for name, it := range map[string]*model.MediaItem{"kept": kept, "under a failed listing": under, "locked": locked, "manual": manual, "other scan path": foreign} {
		if !alive(t, it.ID) {
			t.Errorf("%s was pruned", name)
		}
	}
	if alive(t, sibling.ID) {
		t.Error("the unproduced sibling survived")
	}

	// a scan that produced nothing (empty or failed root) prunes nothing
	if n, err := PruneMediaItems(7, map[uint]bool{}, nil); err != nil || n != 0 || !alive(t, kept.ID) {
		t.Fatalf("empty scan pruned %d (%v)", n, err)
	}
	// a failed root covers everything
	if n, err := PruneMediaItems(7, map[uint]bool{kept.ID: true}, []string{"/"}); err != nil || n != 0 || !alive(t, under.ID) {
		t.Fatalf("failed root pruned %d (%v)", n, err)
	}
}

func scrapedRow(t *testing.T, kind string) *model.MediaItem {
	t.Helper()
	it := row(t, "/lib", "Work", kind)
	now := time.Now()
	it.ScrapedAt, it.Cover, it.Plot, it.ScrapedName, it.ReleaseDate = &now, "thumb:/c.jpg", "plot", "The Work", "2010-01-01"
	it.ExternalIDs, it.Candidates = `{"tmdb":"tv/9"}`, `[{"id":"x"}]`
	it.Source, it.Confidence = model.MediaSourceScraper, 0.9
	if err := UpdateMediaItem(it); err != nil {
		t.Fatal(err)
	}
	return it
}

func rescan(t *testing.T, kind string, trusted bool) *model.MediaItem {
	t.Helper()
	it := &model.MediaItem{MediaType: model.MediaTypeVideo, ScanPathID: 7, FolderPath: "/lib", FileName: "Work", IsFolder: true,
		ScrapedName: "Work", VideoType: kind}
	it.Source, it.Confidence = model.MediaSourceRule, 0.85
	if err := SaveScannedMediaItem(it, trusted); err != nil {
		t.Fatal(err)
	}
	got, err := GetMediaItemByID(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRescanReclassifiesAScrapedRowAndQueuesANewScrape(t *testing.T) {
	rescanDB(t)
	it := scrapedRow(t, model.VideoTypeTV)
	if err := db.Create(&model.MediaEpisode{ItemID: it.ID, FileName: "a.mkv", Season: 1, Episode: 1}).Error; err != nil {
		t.Fatal(err)
	}
	got := rescan(t, model.VideoTypeMovie, true)
	if got.VideoType != model.VideoTypeMovie || got.ScrapedAt != nil || got.ExternalIDs != "" || got.Candidates != "" || got.Source != model.MediaSourceRule || got.Confidence != 0.85 {
		t.Errorf("not re-queued: type %q scraped_at %v ids %q candidates %q source %q conf %v", got.VideoType, got.ScrapedAt, got.ExternalIDs, got.Candidates, got.Source, got.Confidence)
	}
	if got.Cover != "thumb:/c.jpg" || got.Plot != "plot" || got.ScrapedName != "The Work" || got.ReleaseDate != "2010-01-01" {
		t.Errorf("scraped metadata was dropped: %+v", got)
	}
	var left int64
	db.Model(&model.MediaEpisode{}).Where("item_id = ?", it.ID).Count(&left)
	if left != 0 {
		t.Errorf("%d episode rows of a work that is no longer a show", left)
	}
}

func TestRescanKeepsWhatItMustNotTouch(t *testing.T) {
	// same class (tv to anime): the kind follows, the scrape stays
	rescanDB(t)
	scrapedRow(t, model.VideoTypeTV)
	got := rescan(t, model.VideoTypeAnime, true)
	if got.VideoType != model.VideoTypeAnime || got.ScrapedAt == nil || got.ExternalIDs == "" {
		t.Errorf("same class: %q scraped_at %v ids %q", got.VideoType, got.ScrapedAt, got.ExternalIDs)
	}
	// a scan whose listings failed does not replace a kind
	rescanDB(t)
	scrapedRow(t, model.VideoTypeTV)
	if got := rescan(t, model.VideoTypeMovie, false); got.VideoType != model.VideoTypeTV || got.ScrapedAt == nil {
		t.Errorf("untrusted scan changed the row: %q scraped_at %v", got.VideoType, got.ScrapedAt)
	}
	// locked and manual rows stay as they are
	for name, mark := range map[string]func(*model.MediaItem){
		"locked": func(i *model.MediaItem) { i.Locked = true },
		"manual": func(i *model.MediaItem) { i.Source = model.MediaSourceManual },
	} {
		rescanDB(t)
		it := scrapedRow(t, model.VideoTypeTV)
		mark(it)
		if err := UpdateMediaItem(it); err != nil {
			t.Fatal(err)
		}
		if got := rescan(t, model.VideoTypeMovie, true); got.VideoType != model.VideoTypeTV || got.ScrapedAt == nil || got.ExternalIDs == "" {
			t.Errorf("%s row was reclassified: %q scraped_at %v", name, got.VideoType, got.ScrapedAt)
		}
	}
}
