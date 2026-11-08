package media

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func strmRels(ts []StrmTarget) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.RelPath + " <- " + t.RawPath
	}
	return out
}

func TestStrmTargetsMovieFolderWithVersionsPointsAtDefaultVersion(t *testing.T) {
	it := &model.MediaItem{
		MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: "Venom", IsFolder: true,
		ScrapedName: "Venom", ReleaseDate: "2018-10-03", VideoType: model.VideoTypeMovie,
		Episodes: model.VersionsJSON([]string{"Venom.120fps.mkv", "Venom.60fps.mkv"}),
	}
	got := StrmTargets(it)
	if len(got) != 1 {
		t.Fatalf("targets: %v", strmRels(got))
	}
	if got[0].RelPath != "Venom (2018)/Venom (2018).strm" || got[0].RawPath != "/lib/Venom/Venom.120fps.mkv" {
		t.Errorf("got %+v", got[0])
	}
	if got[0].SameDirPath != "/lib/Venom/Venom.120fps.strm" {
		t.Errorf("same dir: %q", got[0].SameDirPath)
	}
}

func TestStrmTargetsFolderUsesEpisodeRowsPerSeason(t *testing.T) {
	it := &model.MediaItem{
		MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: "Show.Complete", IsFolder: true,
		ScrapedName: "Show", VideoType: model.VideoTypeTV, Season: 1,
	}
	eps := []model.MediaEpisode{
		{FileName: "Season 1/a.S01E01.mkv", Season: 1, Episode: 1, Title: "Pilot"},
		{FileName: "Season 1/a.S01E02.mkv", Season: 1, Episode: 2},
		{FileName: "Season 2/a.S02E01.mkv", Season: 2, Episode: 1, Title: "Again: 2/3"},
		{FileName: "Season 2/a.S02E01.v2.mkv", Season: 2, Episode: 1, Version: true},
	}
	got := strmRels(StrmTargetsWithEpisodes(it, eps))
	want := []string{
		"Show/Season 01/Show S01E01 - Pilot.strm <- /lib/Show.Complete/Season 1/a.S01E01.mkv",
		"Show/Season 01/Show S01E02.strm <- /lib/Show.Complete/Season 1/a.S01E02.mkv",
		"Show/Season 02/Show S02E01 - Again_ 2_3.strm <- /lib/Show.Complete/Season 2/a.S02E01.mkv",
	}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, got[i], want[i])
		}
	}
}

func TestStrmTargetsFolderWithoutRowsKeepsJSONBehaviour(t *testing.T) {
	it := &model.MediaItem{
		MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: "Show", IsFolder: true,
		ScrapedName: "Show", VideoType: model.VideoTypeTV, Season: 2,
		Episodes: `[{"file_name":"e1.mkv","index":1,"title":"One"},{"file_name":"e2.mkv","index":0,"title":"Two"}]`,
	}
	got := strmRels(StrmTargetsWithEpisodes(it, nil))
	want := []string{
		"Show/Season 02/Show S02E01 - One.strm <- /lib/Show/e1.mkv",
		"Show/Season 02/Show S02E02 - Two.strm <- /lib/Show/e2.mkv",
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %q", got)
	}
}
