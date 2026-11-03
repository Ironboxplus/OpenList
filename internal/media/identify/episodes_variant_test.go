package identify

import "testing"

func TestEpisodesOfFolderThatHoldReleasesNotSeasons(t *testing.T) {
	tree := map[string][]Entry{
		"":                    dirs("Grandmaster release", "Other release"),
		"Grandmaster release": eps("Show.S01E%02d.mkv", 15),
		"Other release":       eps("Show.S01E%02d.mkv", 12),
	}
	got, err := EpisodeFiles(func(rel string) ([]Entry, error) { return tree[rel], nil })
	if err != nil || len(got) != 15 {
		t.Fatalf("got %d episodes (%v), want the 15 of the larger release", len(got), err)
	}
	for _, f := range got {
		if f.Season != 1 {
			t.Fatalf("season %d", f.Season)
		}
	}
}
