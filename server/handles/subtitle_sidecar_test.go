package handles

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/gin-gonic/gin"
)

func useVideoTypes(t *testing.T) {
	t.Helper()
	old := conf.SlicesMap[conf.VideoTypes]
	conf.SlicesMap[conf.VideoTypes] = []string{"mp4", "mkv", "avi"}
	t.Cleanup(func() { conf.SlicesMap[conf.VideoTypes] = old })
}

func paths(ms []sidecarMatch) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.Dir+"/"+m.Name)
	}
	return out
}

func TestCollectSidecarSubtitles(t *testing.T) {
	useVideoTypes(t)
	tests := []struct {
		name    string
		video   string
		listing map[string]dirListing
		want    []string
	}{
		{
			"(a) same-name only, other video present",
			"A.mkv",
			map[string]dirListing{"": {Files: []string{"A.mkv", "A.zh.srt", "A.en.ass", "B.mkv", "B.srt"}}},
			[]string{"/A.en.ass", "/A.zh.srt"},
		},
		{
			"(b) Subs folder same-name",
			"Movie.mkv",
			map[string]dirListing{
				"":     {Files: []string{"Movie.mkv", "Other.mkv"}, Dirs: []string{"Subs"}},
				"Subs": {Files: []string{"Movie.eng.srt", "Other.srt"}},
			},
			[]string{"Subs/Movie.eng.srt"},
		},
		{
			"(b) case-insensitive folder name and CJK folder",
			"Movie.mkv",
			map[string]dirListing{
				"":          {Files: []string{"Movie.mkv", "Other.mkv"}, Dirs: []string{"SUBTITLES", "字幕"}},
				"SUBTITLES": {Files: []string{"Movie.srt"}},
				"字幕":        {Files: []string{"Movie.chs.ass"}},
			},
			[]string{"SUBTITLES/Movie.srt", "字幕/Movie.chs.ass"},
		},
		{
			"(b) RARBG folder named after video",
			"Movie.2009.mkv",
			map[string]dirListing{
				"":                {Files: []string{"Movie.2009.mkv", "Other.mkv"}, Dirs: []string{"Subs"}},
				"Subs":            {Dirs: []string{"movie.2009", "Other"}},
				"Subs/movie.2009": {Files: []string{"2_English.srt", "3_French.srt", "readme.txt"}},
				"Subs/Other":      {Files: []string{"2_English.srt"}},
			},
			[]string{"Subs/movie.2009/2_English.srt", "Subs/movie.2009/3_French.srt"},
		},
		{
			"(c) single video claims differently named subtitles",
			"Movie.2009.1080p.mkv",
			map[string]dirListing{"": {Files: []string{"Movie.2009.1080p.mkv", "chs.srt", "简体.ass", "poster.jpg"}}},
			[]string{"/chs.srt", "/简体.ass"},
		},
		{
			"(c) two videos: differently named subtitles are not claimed",
			"Movie.mkv",
			map[string]dirListing{"": {Files: []string{"Movie.mkv", "Movie2.mp4", "chs.srt"}}},
			[]string{},
		},
		{
			"(c) second video inside subtitle folder also disables it",
			"Movie.mkv",
			map[string]dirListing{
				"":     {Files: []string{"Movie.mkv", "chs.srt"}, Dirs: []string{"Subs"}},
				"Subs": {Files: []string{"Sample.mkv"}},
			},
			[]string{},
		},
		{
			"(c) single video also takes subtitles from the Subs folder",
			"Movie.mkv",
			map[string]dirListing{
				"":     {Files: []string{"Movie.mkv"}, Dirs: []string{"Subs"}},
				"Subs": {Files: []string{"chs.srt"}},
			},
			[]string{"Subs/chs.srt"},
		},
		{
			"ordering a, b, c and no duplicates",
			"M.mkv",
			map[string]dirListing{
				"":     {Files: []string{"M.mkv", "zz.srt", "M.srt"}, Dirs: []string{"Subs"}},
				"Subs": {Files: []string{"M.en.srt", "aa.srt"}},
			},
			[]string{"/M.srt", "Subs/M.en.srt", "/zz.srt", "Subs/aa.srt"},
		},
		{
			"no subtitles",
			"M.mkv",
			map[string]dirListing{"": {Files: []string{"M.mkv"}}},
			[]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := paths(collectSidecarSubtitles(tt.video, tt.listing))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPendingSubtitleDirs(t *testing.T) {
	listing := map[string]dirListing{"": {Files: []string{"M.mkv"}, Dirs: []string{"Subs", "Extras", "字幕"}}}
	if got, want := pendingSubtitleDirs("M.mkv", listing), []string{"Subs", "字幕"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("round 1: got %v, want %v", got, want)
	}
	listing["Subs"] = dirListing{Dirs: []string{"m", "Other"}}
	listing["字幕"] = dirListing{}
	if got, want := pendingSubtitleDirs("M.mkv", listing), []string{"Subs/m"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("round 2: got %v, want %v", got, want)
	}
	listing["Subs/m"] = dirListing{}
	if got := pendingSubtitleDirs("M.mkv", listing); len(got) != 0 {
		t.Fatalf("round 3: got %v, want none", got)
	}
}

func TestPendingSubtitleDirsNoSubtitleFolder(t *testing.T) {
	listing := map[string]dirListing{"": {Files: []string{"M.mkv"}, Dirs: []string{"Extras"}}}
	if got := pendingSubtitleDirs("M.mkv", listing); len(got) != 0 {
		t.Fatalf("no subtitle folder must cost no extra listing, got %v", got)
	}
}

type fakeSubtitleProvider struct{ calls int }

func (f *fakeSubtitleProvider) VideoSubtitle(context.Context, model.Obj) ([]driver.VideoSubtitleInfo, error) {
	f.calls++
	return []driver.VideoSubtitleInfo{{Title: "x", Type: "srt"}}, nil
}

func TestProviderSubtitlesSidecarOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/api/fs/video_subtitle", nil)

	p := &fakeSubtitleProvider{}
	if got := providerSubtitles(c, p, nil, true); len(got) != 0 || p.calls != 0 {
		t.Fatalf("sidecar_only must not call the provider: got %v, calls %d", got, p.calls)
	}
	got := providerSubtitles(c, p, nil, false)
	if p.calls != 1 || len(got) != 1 || got[0].Source != subtitleSourceProvider {
		t.Fatalf("default must call provider and tag source: got %+v, calls %d", got, p.calls)
	}
	if got := providerSubtitles(c, struct{}{}, nil, false); got != nil {
		t.Fatalf("driver without capability: got %v", got)
	}
}

func TestSubtitleSourceValues(t *testing.T) {
	if subtitleSourceSidecar != "sidecar" || subtitleSourceProvider != "provider" {
		t.Fatal("source values are part of the API")
	}
}
