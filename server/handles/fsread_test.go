package handles

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func TestGetReadme(t *testing.T) {
	tests := []struct {
		name   string
		meta   *model.Meta
		path   string
		want   string
		reason string
	}{
		{
			name:   "nil meta",
			meta:   nil,
			path:   "/any",
			want:   "",
			reason: "nil meta should return empty",
		},
		{
			name: "exact path match with RSub=false",
			meta: &model.Meta{
				Path:   "/folder",
				Readme: "Welcome",
				RSub:   false,
			},
			path:   "/folder",
			want:   "Welcome",
			reason: "exact path should show readme",
		},
		{
			name: "sub path with RSub=true",
			meta: &model.Meta{
				Path:   "/folder",
				Readme: "Welcome",
				RSub:   true,
			},
			path:   "/folder/subfolder",
			want:   "Welcome",
			reason: "sub path with RSub=true should show readme",
		},
		{
			name: "sub path with RSub=false",
			meta: &model.Meta{
				Path:   "/folder",
				Readme: "Welcome",
				RSub:   false,
			},
			path:   "/folder/subfolder",
			want:   "",
			reason: "sub path with RSub=false should not show readme",
		},
		{
			name: "non-sub path with RSub=true (BEHAVIOR CHANGE - BUG FIX)",
			meta: &model.Meta{
				Path:   "/folder",
				Readme: "Welcome",
				RSub:   true,
			},
			path:   "/other",
			want:   "",
			reason: "non-sub path should not show readme even with RSub=true (fixed bug)",
		},
		{
			name: "root readme applies to all with RSub=true",
			meta: &model.Meta{
				Path:   "/",
				Readme: "Global Info",
				RSub:   true,
			},
			path:   "/any/path",
			want:   "Global Info",
			reason: "root readme with RSub=true should apply to all paths",
		},
		{
			name: "empty readme",
			meta: &model.Meta{
				Path:   "/folder",
				Readme: "",
				RSub:   true,
			},
			path:   "/folder",
			want:   "",
			reason: "empty readme should return empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getReadme(tt.meta, tt.path)
			if got != tt.want {
				t.Errorf("getReadme() = %q, want %q\nReason: %s",
					got, tt.want, tt.reason)
			}
		})
	}
}

func TestGetHeader(t *testing.T) {
	tests := []struct {
		name   string
		meta   *model.Meta
		path   string
		want   string
		reason string
	}{
		{
			name:   "nil meta",
			meta:   nil,
			path:   "/any",
			want:   "",
			reason: "nil meta should return empty",
		},
		{
			name: "exact path match with HeaderSub=false",
			meta: &model.Meta{
				Path:      "/folder",
				Header:    "Custom Header",
				HeaderSub: false,
			},
			path:   "/folder",
			want:   "Custom Header",
			reason: "exact path should show header",
		},
		{
			name: "sub path with HeaderSub=true",
			meta: &model.Meta{
				Path:      "/folder",
				Header:    "Custom Header",
				HeaderSub: true,
			},
			path:   "/folder/subfolder",
			want:   "Custom Header",
			reason: "sub path with HeaderSub=true should show header",
		},
		{
			name: "sub path with HeaderSub=false",
			meta: &model.Meta{
				Path:      "/folder",
				Header:    "Custom Header",
				HeaderSub: false,
			},
			path:   "/folder/subfolder",
			want:   "",
			reason: "sub path with HeaderSub=false should not show header",
		},
		{
			name: "non-sub path with HeaderSub=true (BEHAVIOR CHANGE - BUG FIX)",
			meta: &model.Meta{
				Path:      "/folder",
				Header:    "Custom Header",
				HeaderSub: true,
			},
			path:   "/other",
			want:   "",
			reason: "non-sub path should not show header even with HeaderSub=true (fixed bug)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getHeader(tt.meta, tt.path)
			if got != tt.want {
				t.Errorf("getHeader() = %q, want %q\nReason: %s",
					got, tt.want, tt.reason)
			}
		})
	}
}

func TestMatchSidecarSubtitle(t *testing.T) {
	tests := []struct {
		name    string
		video   string
		sibling string
		wantExt string
		wantOK  bool
	}{
		{"plain srt", "Movie.mkv", "Movie.srt", "srt", true},
		{"lang-tagged ass", "Movie.mkv", "Movie.zh.ass", "ass", true},
		{"multi-tag vtt", "Show.S01E01.mkv", "Show.S01E01.eng.forced.vtt", "vtt", true},
		{"sup pgs", "Movie.mkv", "Movie.sup", "sup", true},
		{"ssa", "Movie.mkv", "Movie.SSA", "ssa", true},
		{"case-insensitive both", "MOVIE.MKV", "movie.SRT", "srt", true},
		{"prefix collision excluded", "Movie.mkv", "Movie2.srt", "", false},
		{"different basename", "Movie.mkv", "Other.srt", "", false},
		{"non-subtitle ext", "Movie.mkv", "Movie-poster.jpg", "", false},
		{"video itself", "Movie.mkv", "Movie.mkv", "", false},
		{"basename substring not boundary", "Movie.mkv", "Moviegoers.srt", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotExt, gotOK := matchSidecarSubtitle(tt.video, tt.sibling)
			if gotExt != tt.wantExt || gotOK != tt.wantOK {
				t.Errorf("matchSidecarSubtitle(%q, %q) = (%q, %v), want (%q, %v)",
					tt.video, tt.sibling, gotExt, gotOK, tt.wantExt, tt.wantOK)
			}
		})
	}
}

func TestIsEncrypt(t *testing.T) {
	tests := []struct {
		name   string
		meta   *model.Meta
		path   string
		want   bool
		reason string
	}{
		{
			name:   "nil meta",
			meta:   nil,
			path:   "/any",
			want:   false,
			reason: "nil meta should not be encrypted",
		},
		{
			name: "empty password",
			meta: &model.Meta{
				Path:     "/folder",
				Password: "",
			},
			path:   "/folder",
			want:   false,
			reason: "empty password should not be encrypted",
		},
		{
			name: "exact path match with PSub=false",
			meta: &model.Meta{
				Path:     "/folder",
				Password: "secret",
				PSub:     false,
			},
			path:   "/folder",
			want:   true,
			reason: "exact path with password should be encrypted",
		},
		{
			name: "sub path with PSub=true",
			meta: &model.Meta{
				Path:     "/folder",
				Password: "secret",
				PSub:     true,
			},
			path:   "/folder/subfolder",
			want:   true,
			reason: "sub path with PSub=true should be encrypted",
		},
		{
			name: "sub path with PSub=false",
			meta: &model.Meta{
				Path:     "/folder",
				Password: "secret",
				PSub:     false,
			},
			path:   "/folder/subfolder",
			want:   false,
			reason: "sub path with PSub=false should not be encrypted",
		},
		{
			name: "non-sub path with PSub=true",
			meta: &model.Meta{
				Path:     "/folder",
				Password: "secret",
				PSub:     true,
			},
			path:   "/other",
			want:   false,
			reason: "non-sub path should not be encrypted even with PSub=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isEncrypt(tt.meta, tt.path)
			if got != tt.want {
				t.Errorf("isEncrypt() = %v, want %v\nReason: %s",
					got, tt.want, tt.reason)
			}
		})
	}
}
