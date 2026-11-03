package handles

import (
	"encoding/json"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func filmWithVersions() *model.MediaItem {
	return &model.MediaItem{
		FolderPath: "/MOV/Live", FileName: "怦然心动", IsFolder: true, MediaType: model.MediaTypeVideo,
		VideoType: model.VideoTypeMovie,
		Episodes:  model.VersionsJSON([]string{"Flipped.2010.mkv", "av1_recode_Flipped.2010.mkv"}),
	}
}

func TestFilmWithVersionsPlaysTheFirstOneAndListsAllAsVersions(t *testing.T) {
	it := filmWithVersions()
	if got := MediaItemPlayPath(it); got != "/MOV/Live/怦然心动/Flipped.2010.mkv" {
		t.Fatalf("play path %q", got)
	}
	v := &mediaViewer{rel: identityPath}
	view := v.view(it)
	if view.Versions != 2 {
		t.Fatalf("versions %d, want 2", view.Versions)
	}
	d, err := v.detail(it)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d.EpisodeList)
	if d.EpisodeList == nil || len(*d.EpisodeList) != 2 || !(*d.EpisodeList)[0].Version || (*d.EpisodeList)[1].Episode != 2 {
		t.Fatalf("episode_list %s", b)
	}
}

func TestFilmWithoutVersionsKeepsItsOwnPath(t *testing.T) {
	it := &model.MediaItem{FolderPath: "/MOV/Live", FileName: "x.mkv", MediaType: model.MediaTypeVideo}
	if got := MediaItemPlayPath(it); got != "/MOV/Live/x.mkv" {
		t.Fatalf("play path %q", got)
	}
	if v := (&mediaViewer{rel: identityPath}).view(it); v.Versions != 0 {
		t.Fatalf("versions %d", v.Versions)
	}
}
