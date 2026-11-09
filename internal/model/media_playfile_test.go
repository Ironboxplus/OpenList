package model

import "testing"

func TestItemPlayFile(t *testing.T) {
	// the Episodes JSON buildEpisodesFromFolder writes for a path_merge folder with one video
	single := `[{"file_name":"1917.2019.mkv","index":0,"title":"1917.2019"}]`
	multiNoMark := `[{"file_name":"a.mkv","index":1,"title":"a"},{"file_name":"b.mkv","index":2,"title":"b"}]`
	cases := []struct {
		name     string
		item     *MediaItem
		wantOK   bool
		wantFile string
	}{
		{"versions take the first", &MediaItem{IsFolder: true, Episodes: VersionsJSON([]string{"x.120fps.mkv", "x.60fps.mkv"})}, true, "x.120fps.mkv"},
		{"one unmarked video", &MediaItem{IsFolder: true, Episodes: single}, true, "1917.2019.mkv"},
		{"several unmarked videos are not guessed", &MediaItem{IsFolder: true, Episodes: multiNoMark}, false, ""},
		{"no episodes", &MediaItem{IsFolder: true}, false, ""},
		{"bad json", &MediaItem{IsFolder: true, Episodes: "not json"}, false, ""},
		{"a single file is the caller's business", &MediaItem{IsFolder: false, Episodes: single}, false, ""},
		{"nil", nil, false, ""},
	}
	for _, c := range cases {
		got, ok := ItemPlayFile(c.item)
		if ok != c.wantOK || got.FileName != c.wantFile {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, got.FileName, ok, c.wantFile, c.wantOK)
		}
	}
}
