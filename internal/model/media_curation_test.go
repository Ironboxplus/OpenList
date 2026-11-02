package model

import "testing"

func TestIsEpisodicVideo(t *testing.T) {
	for vt, want := range map[string]bool{
		VideoTypeTV: true, VideoTypeAnime: true,
		VideoTypeMovie: false, VideoTypeCollection: false, VideoTypeIgnore: false, "": false,
	} {
		if got := IsEpisodicVideo(vt); got != want {
			t.Errorf("IsEpisodicVideo(%q) = %v, want %v", vt, got, want)
		}
	}
}

func TestMediaItemIsSeries(t *testing.T) {
	cases := []struct {
		name string
		it   MediaItem
		want bool
	}{
		{"tv folder", MediaItem{VideoType: VideoTypeTV, IsFolder: true}, true},
		{"anime folder", MediaItem{VideoType: VideoTypeAnime, IsFolder: true}, true},
		{"anime single episode file", MediaItem{VideoType: VideoTypeAnime}, true},
		{"movie in its own folder", MediaItem{VideoType: VideoTypeMovie, IsFolder: true}, false},
		{"collection folder", MediaItem{VideoType: VideoTypeCollection, IsFolder: true}, false},
		{"unclassified folder is read as a series", MediaItem{IsFolder: true}, true},
		{"unclassified file is a movie", MediaItem{}, false},
	}
	for _, c := range cases {
		if got := c.it.IsSeries(); got != c.want {
			t.Errorf("%s: IsSeries() = %v, want %v", c.name, got, c.want)
		}
	}
}
