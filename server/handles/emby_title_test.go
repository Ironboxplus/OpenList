package handles

import (
	"encoding/json"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// The names clients are shown follow media_title_source; every id and series key
// stays what it was.
func TestEmbyNamesFollowTheTitleSourceButIdsDoNot(t *testing.T) {
	defer func() { mediaTitleSourceHook = "" }()
	show := model.MediaItem{ID: 1, MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: "[VCB] Another [1080p]", IsFolder: true, FileTitle: "Another", ScrapedName: "另一部"}
	show.VideoType = model.MediaKindTV
	show.Episodes = `[{"file_name":"01.mkv","index":1,"title":"e1"}]`
	film := model.MediaItem{ID: 2, MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: "Inception.2010.mkv", FileTitle: "Inception", ScrapedName: "盗梦空间"}
	film.VideoType = model.MediaKindMovie
	fixed := model.MediaItem{ID: 3, MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: "Fixed.2020.mkv", FileTitle: "Fixed", ScrapedName: "我定的名字", Locked: true}
	fixed.VideoType = model.MediaKindMovie

	type shot struct {
		ids   []string
		names []string
	}
	run := func(src string) shot {
		mediaTitleSourceHook = src
		l := &embyLibrary{videos: []model.MediaItem{show, film, fixed}, ids: map[string]string{}}
		var out shot
		for _, s := range l.seriesList() {
			e := l.seriesItem(s)
			out.ids = append(out.ids, e.Id, s.Key)
			out.names = append(out.names, e.Name)
		}
		for _, m := range []*model.MediaItem{&film, &fixed} {
			e := l.movieItem(m)
			out.ids = append(out.ids, e.Id)
			out.names = append(out.names, e.Name, e.SortName)
		}
		b, _ := json.Marshal(out.ids)
		out.ids = []string{string(b)}
		return out
	}
	file, scraped := run(model.TitleSourceFile), run(model.TitleSourceScraped)
	if file.ids[0] != scraped.ids[0] {
		t.Fatalf("ids differ between the settings:\n file    %s\n scraped %s", file.ids[0], scraped.ids[0])
	}
	if got := file.names; got[0] != "Another" || got[1] != "Inception" || got[2] != "Inception" || got[3] != "我定的名字" {
		t.Errorf("file names: %v", got)
	}
	if got := scraped.names; got[0] != "另一部" || got[1] != "盗梦空间" || got[3] != "我定的名字" {
		t.Errorf("scraped names: %v", got)
	}
}
