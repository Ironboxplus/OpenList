package media

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestSidecarNamesMatchOnVideoNamePlusDot(t *testing.T) {
	names := []string{"x.mkv", "x.ass", "x.chs.ass", "x.zh-CN.forced.srt", "x.nfo", "x.mkv.srt", "xy.srt", "x2.srt", "X.srt", "y.srt", "x.jpg"}
	got := SidecarNames("x.mkv", names)
	want := []string{"x.ass", "x.chs.ass", "x.zh-CN.forced.srt", "x.mkv.srt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	// the video's own name is never a sidecar
	if got := SidecarNames("a.srt.mkv", []string{"a.srt.mkv", "a.srt.srt"}); !reflect.DeepEqual(got, []string{"a.srt.srt"}) {
		t.Errorf("self: %v", got)
	}
}

func TestRecordSidecarsReplacesPerItem(t *testing.T) {
	conf.Conf = conf.DefaultConfig(t.TempDir())
	d, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:media-sidecar-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Init(d)
	show := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: "Show", IsFolder: true, ScrapedName: "Show", VideoType: model.VideoTypeTV}
	file := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: "/lib/films", FileName: "F.mkv", ScrapedName: "F", VideoType: model.VideoTypeMovie}
	for _, it := range []*model.MediaItem{show, file} {
		if err := db.SaveVideoItem(it, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	tree := map[string][]identify.Entry{
		"/lib/Show/Season 1": {{Name: "a.S01E01.mkv"}, {Name: "a.S01E01.chs.ass", Size: 123}, {Name: "a.S01E02.mkv"}, {Name: "a.S01E02.srt"}, {Name: "sub", IsDir: true}},
		"/lib/films":         {{Name: "F.mkv"}, {Name: "F.eng.srt"}, {Name: "G.srt"}},
	}
	list := func(_ context.Context, p string) ([]identify.Entry, error) {
		es, ok := tree[p]
		if !ok {
			return nil, fmt.Errorf("no %s", p)
		}
		return es, nil
	}
	ctx := context.Background()
	recordSidecars(ctx, show, []string{"Season 1/a.S01E01.mkv", "Season 1/a.S01E02.mkv"}, list)
	recordSidecars(ctx, file, []string{"F.mkv"}, list)
	got, err := db.ListMediaSidecars([]uint{show.ID, file.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[show.ID]) != 2 || got[show.ID][0].Video != "Season 1/a.S01E01.mkv" || got[show.ID][0].Path != "Season 1/a.S01E01.chs.ass" || got[show.ID][0].Size != 123 ||
		got[show.ID][1].Path != "Season 1/a.S01E02.srt" {
		t.Errorf("show: %+v", got[show.ID])
	}
	if len(got[file.ID]) != 1 || got[file.ID][0].Video != "F.mkv" || got[file.ID][0].Path != "F.eng.srt" {
		t.Errorf("file: %+v", got[file.ID])
	}

	// a rescan that no longer finds a subtitle drops it; a failed listing keeps the rows
	tree["/lib/Show/Season 1"] = []identify.Entry{{Name: "a.S01E01.mkv"}, {Name: "a.S01E01.chs.ass", Size: 123}, {Name: "a.S01E02.mkv"}}
	recordSidecars(ctx, show, []string{"Season 1/a.S01E01.mkv", "Season 1/a.S01E02.mkv"}, list)
	delete(tree, "/lib/films")
	recordSidecars(ctx, file, []string{"F.mkv"}, list)
	got, _ = db.ListMediaSidecars([]uint{show.ID, file.ID})
	if len(got[show.ID]) != 1 || len(got[file.ID]) != 1 {
		t.Errorf("after rescan: %+v", got)
	}
}
