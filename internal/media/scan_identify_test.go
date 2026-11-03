package media

import (
	"context"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

const gb = int64(1) << 30

func TestApplyIdentifyFillsTheScannedItem(t *testing.T) {
	entries := []identify.Entry{{Name: "Pride.and.Prejudice.2005.mkv", Size: 8 * gb}}
	r := identify.Classify("傲慢与偏见2005", true, entries)
	item := &model.MediaItem{FileName: "傲慢与偏见2005", IsFolder: true, ScrapedName: "傲慢与偏见2005"}
	applyIdentify(item, r)
	if item.VideoType != model.VideoTypeMovie || item.ScrapedName != "傲慢与偏见" || item.ReleaseDate != "2005-01-01" {
		t.Errorf("item = %+v", item)
	}
	if item.Source != model.MediaSourceRule || item.Confidence <= 0 || item.Hidden {
		t.Errorf("curation = %+v hidden=%v", item.MediaCuration, item.Hidden)
	}
}

func TestIdentifyScanItemFileDoesNotListAnything(t *testing.T) {
	// a file has no listing: nothing in this path may touch the file system
	item := &model.MediaItem{FileName: "[ANi] 葬送的芙莉莲 - 07 [1080P][Baha].mkv"}
	entries, r := identifyScanItem(context.Background(), "/lib/x.mkv", item)
	if entries != nil {
		t.Errorf("entries = %v", entries)
	}
	if !model.IsEpisodicVideo(item.VideoType) || item.Episode != 7 || r.Kind != item.VideoType {
		t.Errorf("a fansub episode file must come out as an episode of a series: %+v (%+v)", item, r)
	}
}

func TestSoundtrackFolderIsHiddenAndIgnored(t *testing.T) {
	r := identify.Classify("黑神话.悟空.OST", true, nil)
	item := &model.MediaItem{FileName: "黑神话.悟空.OST", IsFolder: true}
	applyIdentify(item, r)
	if item.VideoType != model.VideoTypeIgnore || !item.Hidden {
		t.Errorf("item = %+v", item)
	}
}

func TestCollectionChildrenSkipsExtras(t *testing.T) {
	entries := []identify.Entry{
		{Name: "Pulp Fiction (1994)", IsDir: true},
		{Name: "Subs", IsDir: true},
		{Name: "bonus.mkv", Size: gb},
		{Name: "poster.jpg"},
	}
	got := collectionChildren("/lib/昆汀合集", entries)
	want := []string{"/lib/昆汀合集/Pulp Fiction (1994)", "/lib/昆汀合集/bonus.mkv"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("children = %v, want %v", got, want)
	}
}

func TestEpisodeFilesOfASeasonedShow(t *testing.T) {
	tree := map[string][]identify.Entry{
		"":         {{Name: "Season 1", IsDir: true}, {Name: "Season 2", IsDir: true}, {Name: "Extras", IsDir: true}, {Name: "readme.txt"}},
		"Season 1": {{Name: "Show.S01E01.mkv", Size: gb}, {Name: "Show.S01E02.mkv", Size: gb}},
		"Season 2": {{Name: "Show.S02E01.mkv", Size: gb}},
	}
	files, err := identify.EpisodeFiles(func(rel string) ([]identify.Entry, error) { return tree[rel], nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[0].Rel != "Season 1/Show.S01E01.mkv" || files[2].Season != 2 {
		t.Fatalf("files = %+v", files)
	}
	eps := make([]model.MediaEpisode, 0, len(files))
	for _, f := range files {
		eps = append(eps, model.MediaEpisode{FileName: f.Rel, Season: f.Season, Episode: f.Episode})
	}
	// two seasons: Index is the position, not the repeating episode number
	infos := model.EpisodeInfos(eps)
	if infos[2].Index != 3 {
		t.Errorf("index of S02E01 = %d, want 3", infos[2].Index)
	}
}

// A folder that cannot be listed gives a verdict from its name alone; the scan
// must be told, so that it neither re-classifies the row nor prunes below it.
func TestIdentifyScanItemCheckedReportsAFailedListing(t *testing.T) {
	item := &model.MediaItem{FileName: "怦然心动", IsFolder: true}
	_, _, err := identifyScanItemChecked(context.Background(), "/no-such-storage/怦然心动", item)
	if err == nil {
		t.Fatal("a failed listing was not reported")
	}
	file := &model.MediaItem{FileName: "x.mkv"}
	if _, _, err := identifyScanItemChecked(context.Background(), "/lib/x.mkv", file); err != nil {
		t.Fatalf("a file has no listing to fail: %v", err)
	}
}

func TestSettleVerdictKeepsADecidedShowAndTitlesMembers(t *testing.T) {
	// 星蝶公主: the rules say several works, the scraper has said one show
	show := func() (*model.MediaItem, identify.Result) {
		item := &model.MediaItem{FolderPath: "/lib/Anime", FileName: "星蝶公主", IsFolder: true}
		r := identify.Result{Kind: "collection", Title: "星蝶公主", FromContent: true, ChildKinds: []string{"tv", "tv"}, Confidence: 0.6}
		applyIdentify(item, r)
		return item, r
	}
	item, r := show()
	settleVerdict(item, &r, false, func(folder, name string) string {
		if folder != "/lib/Anime" || name != "星蝶公主" {
			t.Errorf("asked about %s/%s", folder, name)
		}
		return model.VideoTypeTV
	})
	if r.Kind != "tv" || item.VideoType != "tv" {
		t.Errorf("kind %q / %q, want the decided show", r.Kind, item.VideoType)
	}
	// no decision, or the rules' own: still a collection
	item, r = show()
	settleVerdict(item, &r, false, func(string, string) string { return "" })
	if r.Kind != "collection" || item.VideoType != "collection" {
		t.Errorf("undecided: %q", r.Kind)
	}
	// a firm collection is never asked
	item, r = show()
	r.Firm = true
	settleVerdict(item, &r, false, func(string, string) string { t.Error("asked about a firm collection"); return "tv" })

	// a member named by a number takes the collection's title and keeps its year
	item = &model.MediaItem{FolderPath: "/lib/Live/与神同行", FileName: "2", IsFolder: true}
	r = identify.Classify("2", true, []identify.Entry{{Name: "Along.with.the.Gods.The.Last.49.Days.2018.BluRay.mkv", Size: 12 * gb}})
	applyIdentify(item, r)
	settleVerdict(item, &r, true, func(string, string) string { return "" })
	if item.ScrapedName != "与神同行 2" || item.ReleaseDate != "2018-01-01" {
		t.Errorf("member: %q %q", item.ScrapedName, item.ReleaseDate)
	}
	// a top-level work is not a member
	item = &model.MediaItem{FolderPath: "/lib/Live", FileName: "2", IsFolder: true}
	r = identify.Classify("2", true, nil)
	applyIdentify(item, r)
	settleVerdict(item, &r, false, func(string, string) string { return "" })
	if item.ScrapedName != "2" {
		t.Errorf("top-level work retitled: %q", item.ScrapedName)
	}
}
