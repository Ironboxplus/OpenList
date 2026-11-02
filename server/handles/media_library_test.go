package handles

import (
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

// saveWork stores a video work at p. The title is the base name unless given.
func saveWork(t *testing.T, p, kind, title string, year int, cover string) *model.MediaItem {
	t.Helper()
	folder, name := db.SplitMediaPath(p)
	if title == "" {
		title = name
	}
	it := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: folder, FileName: name, IsFolder: true, ScrapedName: title, Cover: cover}
	it.VideoType = kind
	it.Hidden = kind == model.MediaKindIgnore
	if year > 0 {
		it.ReleaseDate = strconv.Itoa(year) + "-01-01"
	}
	if err := db.SaveVideoItem(it, nil, nil); err != nil {
		t.Fatal(err)
	}
	return it
}

type libPage struct {
	Content []struct {
		Path         string         `json:"path"`
		VideoType    string         `json:"video_type"`
		Ratings      map[string]any `json:"ratings"`
		ChildPosters *[]string      `json:"child_posters"`
		EpisodeList  *[]any         `json:"episode_list"`
	} `json:"content"`
	Total int `json:"total"`
}

func (m *mediaEnv) library(query, user string) (paths []string, page libPage) {
	m.t.Helper()
	w := m.get("/api/fs/media/list?media_type=video"+query, user)
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Code != 200 {
		m.t.Fatalf("list%s: HTTP %d %s", query, w.Code, w.Body.String())
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		m.t.Fatal(err)
	}
	for _, c := range page.Content {
		paths = append(paths, c.Path)
	}
	return paths, page
}

func TestLibraryIsScopedToBasePathAndRelative(t *testing.T) {
	m := coverEnv(t)
	saveWork(t, "/lib/A", "movie", "", 0, "")
	saveWork(t, "/lib/B", "movie", "", 0, "")
	saveWork(t, "/other/C", "movie", "", 0, "")
	paths, page := m.library("", "bob") // bob's base path is /lib
	if page.Total != 2 || !slices.Equal(paths, []string{"/A", "/B"}) {
		t.Fatalf("bob (base /lib) got %v total %d", paths, page.Total)
	}
	if _, page := m.library("", "alice"); page.Total != 3 {
		t.Fatalf("alice (base /) total %d, want 3", page.Total)
	}
}

func TestLibraryHidesWhatTheListingHides(t *testing.T) {
	m := coverEnv(t)
	saveWork(t, "/open/Show", "movie", "", 0, "")
	saveWork(t, "/open/.hidden", "movie", "", 0, "")
	saveWork(t, "/secret/Movie", "movie", "", 0, "")
	saveWork(t, "/readers/Movie", "movie", "", 0, "")
	if err := op.CreateMeta(&model.Meta{Path: "/secret", Password: "pw", PSub: true}); err != nil {
		t.Fatal(err)
	}
	if err := op.CreateMeta(&model.Meta{Path: "/open", Hide: `^.hidden$`, HSub: true}); err != nil {
		t.Fatal(err)
	}
	if err := op.CreateMeta(&model.Meta{Path: "/readers", ReadUsers: []uint{99}, ReadUsersSub: true}); err != nil {
		t.Fatal(err)
	}
	paths, page := m.library("", "alice")
	if page.Total != 1 || !slices.Equal(paths, []string{"/open/Show"}) {
		t.Fatalf("alice sees %v total %d", paths, page.Total)
	}
	// An admin sees hidden names and needs no password, but a read-users list
	// binds everyone not on it, as in the directory listing.
	adminPaths, page := m.library("", "admin")
	slices.Sort(adminPaths)
	if page.Total != 3 || !slices.Equal(adminPaths, []string{"/open/.hidden", "/open/Show", "/secret/Movie"}) {
		t.Fatalf("admin sees %v total %d", adminPaths, page.Total)
	}
}

func TestLibraryFiltersBeforePaging(t *testing.T) {
	m := coverEnv(t)
	// The denied items sort first, so a page cut before the filter would be empty.
	for i := 1; i <= 3; i++ {
		saveWork(t, "/p/"+strconv.Itoa(i), "movie", "a"+strconv.Itoa(i), 0, "")
		saveWork(t, "/q/"+strconv.Itoa(i), "movie", "b"+strconv.Itoa(i), 0, "")
	}
	if err := op.CreateMeta(&model.Meta{Path: "/p", Password: "pw", PSub: true}); err != nil {
		t.Fatal(err)
	}
	p1, page := m.library("&page=1&page_size=2", "alice")
	if page.Total != 3 || !slices.Equal(p1, []string{"/q/1", "/q/2"}) {
		t.Fatalf("page 1: %v total %d", p1, page.Total)
	}
	p2, page := m.library("&page=2&page_size=2", "alice")
	if page.Total != 3 || !slices.Equal(p2, []string{"/q/3"}) {
		t.Fatalf("page 2: %v total %d", p2, page.Total)
	}
}

func TestLibraryKindsPathAndSwitch(t *testing.T) {
	m := coverEnv(t)
	saveWork(t, "/m", "movie", "", 0, "")
	saveWork(t, "/t", "tv", "", 0, "")
	saveWork(t, "/a", "anime", "", 0, "")
	saveWork(t, "/c", "collection", "", 0, "")
	saveWork(t, "/i", "ignore", "", 0, "") // ignored works are hidden
	paths, page := m.library("", "alice")
	slices.Sort(paths)
	if page.Total != 4 || !slices.Equal(paths, []string{"/a", "/c", "/m", "/t"}) {
		t.Fatalf("default: %v total %d", paths, page.Total)
	}
	paths, page = m.library("&kind=tv,anime", "alice")
	slices.Sort(paths)
	if page.Total != 2 || !slices.Equal(paths, []string{"/a", "/t"}) {
		t.Fatalf("kind=tv,anime: %v", paths)
	}
	if page.Content[0].VideoType == "" {
		t.Error("video_type missing from an item")
	}

	// path lists what sits directly in a folder, relative to the user's base path
	saveWork(t, "/lib/C/a", "movie", "", 0, "")
	saveWork(t, "/lib/C/b", "movie", "", 0, "")
	saveWork(t, "/lib/C/deep/x", "movie", "", 0, "")
	paths, _ = m.library("&path="+url.QueryEscape("/lib/C"), "alice")
	if !slices.Equal(paths, []string{"/lib/C/a", "/lib/C/b"}) {
		t.Fatalf("path=/lib/C (alice): %v", paths)
	}
	paths, _ = m.library("&path="+url.QueryEscape("/C"), "bob")
	if !slices.Equal(paths, []string{"/C/a", "/C/b"}) {
		t.Fatalf("path=/C (bob, base /lib): %v", paths)
	}
	if paths, _ = m.library("&path="+url.QueryEscape("/../secret"), "bob"); len(paths) != 0 {
		t.Fatalf("a path out of the base path listed %v", paths)
	}

}

func TestCollectionsGetAChildPosterMosaic(t *testing.T) {
	m := coverEnv(t)
	png := smallPNG(t)
	saveWork(t, "/lib/C", "collection", "", 0, "")
	saveWork(t, "/lib/D", "collection", "", 0, "")
	saveWork(t, "/lib/M", "movie", "", 0, "")
	// five posters under C (one nested deeper), named by release year
	for _, k := range []struct {
		path string
		year int
		file string
	}{{"/lib/C/a", 2005, "y2005.jpg"}, {"/lib/C/b", 1999, "y1999.jpg"}, {"/lib/C/deep/c", 2010, "y2010.jpg"}, {"/lib/C/d", 2001, "y2001.jpg"}, {"/lib/C/e", 2003, "y2003.jpg"}, {"/lib/C/f", 0, "y0.jpg"}} {
		saveWork(t, k.path, "movie", "", k.year, writeThumb(t, k.file, png))
	}
	saveWork(t, "/lib/C/nopic", "movie", "", 1990, "") // without a poster it adds nothing

	posterFile := func(u string) string {
		pu, err := url.Parse(u)
		if err != nil || pu.Path != "/api/public/media/thumb" {
			t.Fatalf("not a thumb url: %q", u)
		}
		return pu.Query().Get("path")
	}
	_, page := m.library("&path="+url.QueryEscape("/lib"), "alice")
	by := map[string]int{}
	for i, c := range page.Content {
		by[c.Path] = i
	}
	c := page.Content[by["/lib/C"]]
	if c.ChildPosters == nil || len(*c.ChildPosters) != 4 {
		t.Fatalf("C child_posters: %+v", c.ChildPosters)
	}
	for i, want := range []string{"y1999", "y2001", "y2003", "y2005"} {
		if got := posterFile(strings.TrimSpace((*c.ChildPosters)[i])); got != "/imgs/"+want+".jpg" {
			t.Errorf("poster %d is %s, want the one of %s", i, got, want)
		}
	}
	if d := page.Content[by["/lib/D"]]; d.ChildPosters == nil || len(*d.ChildPosters) != 0 {
		t.Errorf("an empty collection must carry an empty child_posters, got %v", d.ChildPosters)
	}
	if mv := page.Content[by["/lib/M"]]; mv.ChildPosters != nil {
		t.Error("a movie got child_posters")
	}

	// a poster of something the user may not read is left out
	if err := op.CreateMeta(&model.Meta{Path: "/lib/C/b", ReadUsers: []uint{99}, ReadUsersSub: true}); err != nil {
		t.Fatal(err)
	}
	_, page = m.library("&path="+url.QueryEscape("/lib"), "alice")
	by = map[string]int{}
	for i, c := range page.Content {
		by[c.Path] = i
	}
	cp := page.Content[by["/lib/C"]].ChildPosters
	if cp == nil || len(*cp) != 4 || posterFile((*cp)[0]) != "/imgs/y2001.jpg" {
		t.Errorf("an unreadable item's poster is in the mosaic: %v", cp)
	}
}

func TestDetailHasRatingsEpisodesAndAdminOnlyCandidates(t *testing.T) {
	m := coverEnv(t)
	it := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: "Show", IsFolder: true, ScrapedName: "Show"}
	it.VideoType = "tv"
	it.Candidates = `[{"provider":"tmdb","id":"tv/1","title":"cand","score":0.5}]`
	err := db.SaveVideoItem(it,
		[]model.MediaRating{{Source: "tmdb", Value: 8.1}, {Source: "rt", Text: "93%"}},
		[]model.MediaEpisode{
			{FileName: "S01E02.mkv", Season: 1, Episode: 2, Title: "two", Still: writeThumb(t, "still.jpg", smallPNG(t))},
			{FileName: "S01E01.mkv", Season: 1, Episode: 1, Title: "one"},
		})
	if err != nil {
		t.Fatal(err)
	}
	id := "/api/fs/media/item/" + strconv.Itoa(int(it.ID))

	var got struct {
		Path        string         `json:"path"`
		Ratings     map[string]any `json:"ratings"`
		EpisodeList []struct {
			Title    string `json:"title"`
			StillURL string `json:"still_url"`
		} `json:"episode_list"`
	}
	raw := m.ok("GET", id, "alice", nil)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != "/lib/Show" || got.Ratings["rt"] != "93%" || got.Ratings["tmdb"] == nil {
		t.Fatalf("detail: %s", raw)
	}
	if len(got.EpisodeList) != 2 || got.EpisodeList[0].Title != "one" || !strings.HasPrefix(got.EpisodeList[1].StillURL, "/api/public/media/thumb?") {
		t.Fatalf("episode_list: %s", raw)
	}
	// candidates travel as a JSON string, so their quotes are escaped in the body
	if strings.Contains(string(raw), "provider") || !strings.Contains(string(raw), `"candidates":""`) {
		t.Fatalf("candidates visible to a non-admin: %s", raw)
	}
	if !strings.Contains(string(m.ok("GET", id, "admin", nil)), "provider") {
		t.Fatal("an admin does not get the candidates")
	}

	// the list carries ratings but neither the episode list nor the candidates
	_, page := m.library("", "alice")
	if len(page.Content) != 1 || page.Content[0].Ratings["tmdb"] == nil || page.Content[0].EpisodeList != nil {
		t.Fatalf("list item: %+v", page.Content)
	}
	w := m.get("/api/fs/media/list?media_type=video", "alice")
	if strings.Contains(w.Body.String(), "provider") {
		t.Fatalf("candidates in the list for a non-admin: %s", w.Body.String())
	}
}

func TestByPath(t *testing.T) {
	m := coverEnv(t)
	it := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: "/lib", FileName: "Show", IsFolder: true, ScrapedName: "Show"}
	it.VideoType = "tv"
	it.Candidates = `[{"provider":"tmdb","id":"tv/1","title":"cand","score":0.5}]`
	if err := db.SaveVideoItem(it, nil, []model.MediaEpisode{{FileName: "S01E01.mkv", Season: 1, Episode: 1, Title: "one"}}); err != nil {
		t.Fatal(err)
	}
	saveWork(t, "/other/Away", "movie", "", 0, "")
	saveWork(t, "/secret/Movie", "movie", "", 0, "")
	if err := op.CreateMeta(&model.Meta{Path: "/secret", Password: "pw", PSub: true}); err != nil {
		t.Fatal(err)
	}
	saveWork(t, "/lib/.gone", model.MediaKindIgnore, "", 0, "") // hidden item

	notFound := func(path, user string) {
		t.Helper()
		env, w := m.call("GET", "/api/fs/media/by_path?path="+url.QueryEscape(path), user, nil)
		if w.Code != 404 && env.Code != 404 {
			t.Fatalf("%s as %s: HTTP %d code %d %s, want 404", path, user, w.Code, env.Code, w.Body.String())
		}
	}

	// a hit gives what item/:id gives
	var got struct {
		Path        string `json:"path"`
		EpisodeList []struct {
			Title string `json:"title"`
		} `json:"episode_list"`
	}
	raw := m.ok("GET", "/api/fs/media/by_path?path="+url.QueryEscape("/lib/Show"), "alice", nil)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != "/lib/Show" || len(got.EpisodeList) != 1 || got.EpisodeList[0].Title != "one" {
		t.Fatalf("hit: %s", raw)
	}
	byID := m.ok("GET", "/api/fs/media/item/"+strconv.Itoa(int(it.ID)), "alice", nil)
	if string(raw) != string(byID) {
		t.Fatalf("by_path differs from item/:id:\n%s\n%s", raw, byID)
	}

	// no candidates for a non-admin, they stay for an admin
	if strings.Contains(string(raw), "provider") {
		t.Fatalf("candidates visible to a non-admin: %s", raw)
	}
	if !strings.Contains(string(m.ok("GET", "/api/fs/media/by_path?path="+url.QueryEscape("/lib/Show"), "admin", nil)), "provider") {
		t.Fatal("an admin does not get the candidates")
	}

	// the path is relative to the base path (bob's is /lib)
	raw = m.ok("GET", "/api/fs/media/by_path?path=/Show", "bob", nil)
	if err := json.Unmarshal(raw, &got); err != nil || got.Path != "/Show" {
		t.Fatalf("relative hit: %v %s", err, raw)
	}

	notFound("/lib/Nope", "alice")     // miss
	notFound("/other/Away", "bob")     // outside base_path (joins to /lib/other/Away)
	notFound("/../other/Away", "bob")  // cannot climb out of base_path
	notFound("/secret/Movie", "alice") // behind a password
	notFound("/lib/.gone", "alice")    // hidden item
	notFound("", "alice")              // no path
}
