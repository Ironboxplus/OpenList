package apply

import (
	"context"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/nfo"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

type putLog struct{ calls []string }

func (p *putLog) put(_ context.Context, dir, name string, data []byte) error {
	p.calls = append(p.calls, dir+"/"+name+"\n"+string(data))
	return nil
}

func fixed(name, vt string, folder bool, ids string) model.MediaItem {
	it := model.MediaItem{FolderPath: "/lib", FileName: name, IsFolder: folder, ScrapedName: name, ReleaseDate: "2010-07-16"}
	it.VideoType, it.ExternalIDs, it.Locked = vt, ids, true
	return it
}

func TestExportNFOWritesOnlyFixedWorksWithoutAnNFO(t *testing.T) {
	l := &nfoLister{dirs: map[string][]identify.Entry{
		"/lib/盗梦":   {{Name: "a.mkv", Size: 9}},
		"/lib/剧":    {{Name: "e01.mkv", Size: 9}},
		"/lib/有了":   {{Name: "tvshow.nfo", Size: 9}, {Name: "e01.mkv", Size: 9}},
		"/lib/未锁":   {{Name: "e01.mkv", Size: 9}},
		"/lib/没有id": {{Name: "e01.mkv", Size: 9}},
		"/lib":      {{Name: "Film.mkv", Size: 9}},
	}}
	unlocked := fixed("未锁", "tv", true, `{"tmdb":"tv/1"}`)
	unlocked.Locked = false
	items := []model.MediaItem{
		fixed("盗梦", "movie", true, `{"tmdb":"movie/27205","imdb":"tt1375666"}`),
		fixed("剧", "tv", true, `{"tmdb":"tv/1399"}`),
		fixed("有了", "tv", true, `{"tmdb":"tv/5"}`),
		unlocked,
		fixed("没有id", "tv", true, `{"bgm":"265"}`),
		fixed("Film.mkv", "movie", false, `{"tmdb":"movie/9"}`),
	}
	p := &putLog{}

	dry := ExportNFO(context.Background(), l, p.put, items, false)
	if len(p.calls) != 0 {
		t.Fatalf("dry run wrote: %v", p.calls)
	}
	want := []string{"write", "write", "skip", "skip", "skip", "write"}
	for i, r := range dry {
		if r.Action != want[i] {
			t.Errorf("%s: %s (%s), want %s", r.Path, r.Action, r.Reason, want[i])
		}
	}

	got := ExportNFO(context.Background(), l, p.put, items, true)
	if len(p.calls) != 3 {
		t.Fatalf("wrote %d files: %v", len(p.calls), p.calls)
	}
	if got[0].Action != "written" || got[0].File != "movie.nfo" || got[1].File != "tvshow.nfo" || got[5].File != "Film.nfo" {
		t.Errorf("results: %+v", got)
	}
	// what was written reads back to the same ids
	body := p.calls[0][len("/lib/盗梦/movie.nfo\n"):]
	info, err := nfo.Parse([]byte(body))
	if err != nil || info.TMDB != "27205" || info.IMDb != "tt1375666" || info.Kind != "movie" || info.Year != 2010 {
		t.Errorf("read back %+v %v\n%s", info, err, body)
	}
}
