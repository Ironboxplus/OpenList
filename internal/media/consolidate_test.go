package media

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestSiblingReleasesOfOneFilmAreFoldedIntoVersions(t *testing.T) {
	conf.Conf = conf.DefaultConfig(t.TempDir())
	d, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:media-consolidate-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Init(d)
	save := func(name, tmdb string) *model.MediaItem {
		it := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: name, ScrapedName: name, IsFolder: true}
		it.VideoType = model.MediaKindMovie
		it.ExternalIDs = `{"tmdb":"` + tmdb + `"}`
		it.Source = model.MediaSourceScraper
		if err := db.SaveVideoItem(it, nil, nil); err != nil {
			t.Fatal(err)
		}
		return it
	}
	a := save("Venom 60fps", "movie/912649")
	b := save("Venom 120fps", "movie/912649")
	c := save("Other film", "movie/1")
	// a favourite on the folded work moves to the survivor
	if err := d.Create(&model.MediaUserData{UserID: 7, ItemID: b.ID, IsFavorite: true}).Error; err != nil {
		t.Fatal(err)
	}
	tree := map[string][]identify.Entry{
		"/lib/Venom 60fps":  {{Name: "Venom.60fps.mkv", Size: 3 << 30}, {Name: "ad.mkv", Size: 1 << 20}},
		"/lib/Venom 120fps": {{Name: "Venom.120fps.mkv", Size: 4 << 30}},
		"/lib/Other film":   {{Name: "o.mkv", Size: 1 << 30}},
	}
	list := func(_ context.Context, p string) ([]identify.Entry, error) { return tree[p], nil }
	for i := 0; i < 2; i++ { // twice: repeating changes nothing
		if err := consolidateFolder(context.Background(), "/lib", list); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := db.GetMediaItemByID(a.ID)
	vs := model.ItemVersions(got.Episodes)
	if len(vs) != 2 || vs[0].FileName != "../Venom 120fps/Venom.120fps.mkv" || vs[1].FileName != "Venom.60fps.mkv" {
		t.Fatalf("versions %+v", vs)
	}
	if got.Hidden {
		t.Fatal("the survivor is hidden")
	}
	if folded, _ := db.GetMediaItemByID(b.ID); !folded.Hidden {
		t.Fatal("the folded work is still listed")
	}
	if other, _ := db.GetMediaItemByID(c.ID); other.Hidden || other.Episodes != "" {
		t.Fatalf("an unrelated work was touched: %+v", other)
	}
	var fav model.MediaUserData
	if err := d.Where("user_id = ? AND item_id = ?", 7, a.ID).First(&fav).Error; err != nil || !fav.IsFavorite {
		t.Fatalf("favourite did not move: %v %+v", err, fav)
	}
}

func TestReleasesOfOneShowAreFoldedAndSeasonsAreFoldedIntoTheirFolder(t *testing.T) {
	conf.Conf = conf.DefaultConfig(t.TempDir())
	d, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:media-consolidate-tv-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Init(d)
	save := func(folder, name, kind, tmdb string) *model.MediaItem {
		it := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: folder, FileName: name, ScrapedName: name, IsFolder: true}
		it.VideoType = kind
		it.ExternalIDs = `{"tmdb":"` + tmdb + `"}`
		it.Source = model.MediaSourceScraper
		if tmdb == "" {
			it.ExternalIDs, it.Source = "", model.MediaSourceRule
		}
		if err := db.SaveVideoItem(it, nil, nil); err != nil {
			t.Fatal(err)
		}
		return it
	}
	ep := func(pattern string, n int) []identify.Entry {
		var es []identify.Entry
		for i := 1; i <= n; i++ {
			es = append(es, identify.Entry{Name: fmt.Sprintf(pattern, i), Size: 1 << 30})
		}
		return es
	}
	// two releases of one show: VCB has 3 episodes, H-Enc has 4
	vcb := save("/lib", "[VCB-Studio] Kaifuku [Ma10p_1080p]", "anime", "tv/99071")
	henc := save("/lib", "[H-Enc] Kaifuku (BDRip 1080p)", "anime", "tv/99071")
	// two seasons under a folder the rules took for a collection
	coll := save("/lib", "Log Horizon", model.MediaKindCollection, "")
	s1 := save("/lib/Log Horizon", "[VCB-Studio] Log Horizon [Hi10p_1080p]", "anime", "tv/32692")
	s2 := save("/lib/Log Horizon", "[VCB-Studio] Log Horizon 2 [Ma10p_1080p]", "anime", "tv/32692")
	tree := map[string][]identify.Entry{
		"/lib/[VCB-Studio] Kaifuku [Ma10p_1080p]": ep("[VCB-Studio] Kaifuku [%02d][Ma10p_1080p].mkv", 3),
		"/lib/[H-Enc] Kaifuku (BDRip 1080p)":      ep("Kaifuku - %02d.mkv", 4),
		"/lib/Log Horizon":                        {{Name: s1.FileName, IsDir: true}, {Name: s2.FileName, IsDir: true}},
		"/lib/Log Horizon/" + s1.FileName:         ep("[VCB-Studio] Log Horizon [%02d][Hi10p_1080p].mkv", 3),
		"/lib/Log Horizon/" + s2.FileName:         ep("[VCB-Studio] Log Horizon 2 [%02d][Ma10p_1080p].mkv", 2),
	}
	list := func(_ context.Context, p string) ([]identify.Entry, error) { return tree[p], nil }
	for i := 0; i < 2; i++ {
		for _, f := range []string{"/lib", "/lib/Log Horizon"} {
			if err := consolidateFolder(context.Background(), f, list); err != nil {
				t.Fatal(err)
			}
		}
	}
	eps, _ := db.ListMediaEpisodes(vcb.ID)
	versions := 0
	for _, e := range eps {
		if e.Version {
			versions++
		}
	}
	if len(eps) != 7 || versions != 3 {
		t.Fatalf("VCB has %d files, %d of them versions; want 7 and 3: %+v", len(eps), versions, eps)
	}
	if h, _ := db.GetMediaItemByID(henc.ID); !h.Hidden {
		t.Fatal("the other release is still listed")
	}
	p, _ := db.GetMediaItemByID(coll.ID)
	if p.VideoType != "anime" || p.ExternalIDs == "" {
		t.Fatalf("the folder did not become the show: %+v", p)
	}
	seasons, _ := db.ListMediaEpisodes(coll.ID)
	per := map[int]int{}
	for _, e := range seasons {
		per[e.Season]++
	}
	if per[1] != 3 || per[2] != 2 {
		t.Fatalf("episodes per season %v", per)
	}
	if m, _ := db.GetMediaItemByID(s2.ID); !m.Hidden {
		t.Fatal("a season is still listed beside the show")
	}
}
