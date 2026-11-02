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
