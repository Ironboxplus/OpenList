package handles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/bootstrap/data"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/media/apply"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/OpenListTeam/OpenList/v4/server/middlewares"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Fake credentials: the real ones never appear in a test.
const (
	fakeTMDBSecret  = "tmdb-secret-fake-0001"
	fakeOMDbSecret  = "omdb-secret-fake-0002"
	fakeDoubanKey   = "douban-secret-fake-0003"
	fakeProxySecret = "http://proxyuser:proxy-secret-fake-0004@127.0.0.1:1"
)

type mediaEnv struct {
	t      *testing.T
	engine *gin.Engine
}

// newMediaEnv builds a database with the real initial settings (private ones
// filled with fake secrets), a temp asset directory and a router carrying the
// media routes behind the real admin check. The user comes from X-User.
func newMediaEnv(t *testing.T) *mediaEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	conf.Conf = conf.DefaultConfig(t.TempDir())
	name := fmt.Sprintf("file:media-handles-%d?mode=memory&cache=shared", time.Now().UnixNano())
	d, err := gorm.Open(sqlite.Open(name), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Init(d)

	items := data.InitialSettings()
	for i := range items {
		items[i].Index = uint(i)
		switch items[i].Key {
		case conf.Token:
			items[i].Value = "test-admin-token"
		case conf.MediaTMDBKey:
			items[i].Value = fakeTMDBSecret
		case conf.MediaOMDbKeys:
			items[i].Value = fakeOMDbSecret
		case conf.MediaDoubanAPIKey:
			items[i].Value = fakeDoubanKey
		case conf.MediaHTTPProxy:
			items[i].Value = fakeProxySecret
		}
	}
	if err := op.SaveSettingItems(items); err != nil {
		t.Fatal(err)
	}

	oldHook, oldPriv, oldDataDir := mediaConfigHook, mediaAllowPrivateDownloads, conf.DataDir
	conf.DataDir = t.TempDir() // runtime thumbnails land here
	mediaAllowPrivateDownloads = true
	t.Cleanup(func() { mediaConfigHook, mediaAllowPrivateDownloads, conf.DataDir = oldHook, oldPriv, oldDataDir })

	e := gin.New()
	e.Use(func(c *gin.Context) {
		var u *model.User
		switch c.GetHeader("X-User") {
		case "admin":
			u = &model.User{ID: 1, Username: "admin", Role: model.ADMIN, BasePath: "/", Permission: 0x1F}
		case "alice":
			u = &model.User{ID: 2, Username: "alice", Role: model.GENERAL, BasePath: "/", Permission: 0}
		case "bob":
			u = &model.User{ID: 3, Username: "bob", Role: model.GENERAL, BasePath: "/lib", Permission: 0}
		default:
			u = &model.User{ID: 4, Username: "guest", Role: model.GUEST, BasePath: "/", Permission: 0}
		}
		common.GinAppendValues(c, conf.UserKey, u)
		c.Next()
	})
	api := e.Group("/api")
	api.Any("/public/settings", PublicSettings)
	ad := api.Group("/admin", middlewares.AuthAdmin)
	ad.GET("/stats/overview", StatsOverview)
	cu := ad.Group("/media/curate")
	cu.POST("/set", CurateSet)
	cu.POST("/delete", CurateDelete)
	cu.GET("/list", CurateList)
	cu.POST("/image", CurateImage)
	cu.POST("/scrape", CurateScrape)
	cu.GET("/search", CurateSearch)
	api.GET("/public/media/thumb", PublicMediaThumb)
	api.GET("/public/media/proxy", PublicMediaProxy)
	pub := api.Group("/fs/media")
	pub.GET("/list", PublicListMedia)
	pub.GET("/item/:id", PublicGetMedia)
	pub.GET("/albums", PublicListAlbums)
	pub.GET("/album", PublicGetAlbum)
	return &mediaEnv{t: t, engine: e}
}

func (m *mediaEnv) noProxy() {
	m.t.Helper()
	if err := op.SaveSettingItem(&model.SettingItem{Key: conf.MediaHTTPProxy, Value: "", Type: conf.TypeString, Group: model.MEDIA, Flag: model.PRIVATE}); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mediaEnv) do(method, path, user string, body any) *httptest.ResponseRecorder {
	m.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-User", user)
	}
	w := httptest.NewRecorder()
	m.engine.ServeHTTP(w, req)
	return w
}

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (m *mediaEnv) call(method, path, user string, body any) (envelope, *httptest.ResponseRecorder) {
	m.t.Helper()
	w := m.do(method, path, user, body)
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		m.t.Fatalf("%s %s: HTTP %d, body not JSON: %q", method, path, w.Code, w.Body.String())
	}
	return env, w
}

func (m *mediaEnv) ok(method, path, user string, body any) json.RawMessage {
	m.t.Helper()
	env, _ := m.call(method, path, user, body)
	if env.Code != 200 {
		m.t.Fatalf("%s %s: code %d: %s", method, path, env.Code, env.Message)
	}
	return env.Data
}

func testPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sp(s string) *string { return &s }

// ---- tests ----

func TestPrivateMediaSettingsNeverPublic(t *testing.T) {
	m := newMediaEnv(t)
	w := m.do("GET", "/api/public/settings", "", nil)
	body := w.Body.String()
	for _, key := range []string{conf.MediaTMDBKey, conf.MediaOMDbKeys, conf.MediaDoubanAPIKey, conf.MediaHTTPProxy} {
		if strings.Contains(body, key) {
			t.Errorf("private setting %q is in /api/public/settings", key)
		}
	}
	for _, secret := range []string{fakeTMDBSecret, fakeOMDbSecret, fakeDoubanKey, "proxy-secret-fake"} {
		if strings.Contains(body, secret) {
			t.Errorf("a secret value (%q) is in /api/public/settings", secret)
		}
	}
	for _, key := range []string{conf.MediaEnabled, conf.MediaScraperTimeout} {
		if !strings.Contains(body, key) {
			t.Errorf("public setting %q missing from /api/public/settings", key)
		}
	}
	// and the declared flags are what the spec says
	flags := map[string]int{}
	for _, it := range data.InitialSettings() {
		if it.Group == model.MEDIA {
			flags[it.Key] = it.Flag
		}
	}
	want := map[string]int{
		conf.MediaEnabled: model.PUBLIC, conf.MediaTMDBKey: model.PRIVATE, conf.MediaOMDbKeys: model.PRIVATE,
		conf.MediaDoubanAPIKey: model.PRIVATE, conf.MediaHTTPProxy: model.PRIVATE,
		conf.MediaScraperTimeout: model.PUBLIC,
	}
	for k, f := range want {
		if got, ok := flags[k]; !ok || got != f {
			t.Errorf("setting %s: flag %d (declared %v), want %d", k, got, ok, f)
		}
	}
}

func TestAdminMediaEndpointsRejectOtherUsers(t *testing.T) {
	m := newMediaEnv(t)
	for _, user := range []string{"alice", "bob", ""} {
		for _, ep := range []struct{ method, path string }{
			{"POST", "/api/admin/media/curate/set"}, {"POST", "/api/admin/media/curate/delete"}, {"GET", "/api/admin/media/curate/list"},
			{"POST", "/api/admin/media/curate/image?path=/x"}, {"POST", "/api/admin/media/curate/scrape"}, {"GET", "/api/admin/media/curate/search?q=x"},
		} {
			env, _ := m.call(ep.method, ep.path, user, map[string]any{"path": "/x", "kind": "movie"})
			if env.Code == 200 {
				t.Errorf("%s %s answered 200 for user %q", ep.method, ep.path, user)
			}
		}
	}
	if it, _ := db.GetVideoItemByPath("/x"); it != nil {
		t.Fatal("a non-admin created an item")
	}
}

func TestSetOverwritesOnlyGivenFields(t *testing.T) {
	m := newMediaEnv(t)
	m.ok("POST", "/api/admin/media/curate/set", "admin", map[string]any{
		"path": "/lib/Show", "kind": "tv", "title": "Show", "year": 2001, "overview": "keep me",
		"external_ids": map[string]string{"tmdb": "tv/1"}, "genres": []string{"a"},
	})
	m.ok("POST", "/api/admin/media/curate/set", "admin", map[string]any{"path": "/lib/Show", "title": "Show 2", "locked": true})
	got, _ := db.GetVideoItemByPath("/lib/Show")
	if got.ScrapedName != "Show 2" || !got.Locked {
		t.Fatalf("given fields: %+v", got)
	}
	if got.VideoType != "tv" || got.ReleaseDate != "2001-01-01" || got.Plot != "keep me" || got.ExternalIDs != `{"tmdb":"tv/1"}` || got.Genre != "a" || got.ExternalID != "tmdb:1" {
		t.Fatalf("fields not in the request changed: %+v", got)
	}
	env, _ := m.call("POST", "/api/admin/media/curate/set", "admin", map[string]any{"path": "/lib/Show", "kind": "library"})
	if env.Code == 200 {
		t.Fatal("kind library is not a kind of work any more")
	}
}

func TestDeleteAndPendingList(t *testing.T) {
	m := newMediaEnv(t)
	set := func(p string, conf float64, locked bool) {
		m.ok("POST", "/api/admin/media/curate/set", "admin", map[string]any{"path": p, "kind": "tv", "confidence": conf, "locked": locked})
	}
	set("/d/a", 0.3, false)
	set("/d/b", 0.9, false)
	set("/d/c", 0.2, true)
	set("/d/a/s1", 0.9, false)
	set("/e/z", 0.1, false)

	raw := m.ok("GET", "/api/admin/media/curate/list?prefix=/d&pending=true", "admin", nil)
	var l struct {
		Content []struct {
			Path string `json:"path"`
		} `json:"content"`
		Total int `json:"total"`
	}
	_ = json.Unmarshal(raw, &l)
	if l.Total != 1 || len(l.Content) != 1 || l.Content[0].Path != "/d/a" {
		t.Fatalf("pending under /d = %s", raw)
	}
	raw = m.ok("GET", "/api/admin/media/curate/list?kind=tv&per_page=2&page=2", "admin", nil)
	_ = json.Unmarshal(raw, &l)
	if l.Total != 5 || len(l.Content) != 2 {
		t.Fatalf("paging: %s", raw)
	}

	if env, _ := m.call("POST", "/api/admin/media/curate/delete", "admin", map[string]any{"prefix": "/", "recursive": true}); env.Code == 200 {
		t.Fatal("deleting everything must be refused")
	}
	var del struct{ Deleted int }
	_ = json.Unmarshal(m.ok("POST", "/api/admin/media/curate/delete", "admin", map[string]any{"prefix": "/d", "recursive": true}), &del)
	if del.Deleted != 4 {
		t.Fatalf("deleted %d, want 4", del.Deleted)
	}
	if it, _ := db.GetVideoItemByPath("/e/z"); it == nil {
		t.Fatal("recursive delete went outside the prefix")
	}
	_ = json.Unmarshal(m.ok("POST", "/api/admin/media/curate/delete", "admin", map[string]any{"path": "/e/z"}), &del)
	if del.Deleted != 1 {
		t.Fatalf("single delete = %d", del.Deleted)
	}
}

// fakeProviders serves TMDB (from the recorded Evangelion answers), an image
// host, and counts the requests.
func fakeProviders(t *testing.T, m *mediaEnv) (hits *int) {
	m.noProxy()
	t.Helper()
	rec := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "internal", "media", "scraper", "testdata", "recorded", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	poster := testPNG(t, 500, 750, color.RGBA{7, 7, 7, 255})
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/3/tv/890":
			_, _ = w.Write(rec("api_themoviedb_org_3_tv_890.json"))
		case r.URL.Path == "/3/tv/890/season/1":
			_, _ = w.Write(rec("api_themoviedb_org_3_tv_890_season_1.json"))
		case strings.HasPrefix(r.URL.Path, "/3/search/"):
			_, _ = w.Write([]byte(`{"results":[{"id":890,"name":"新世纪福音战士","first_air_date":"1995-10-04","vote_average":8.5}]}`))
		case strings.HasPrefix(r.URL.Path, "/img/"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(poster)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	mediaConfigHook = func(c *scraper.Config) {
		c.BaseURLs = map[string]string{"tmdb": srv.URL + "/3", "tmdb_image": srv.URL + "/img", "omdb": srv.URL, "douban": srv.URL}
		c.RatePerSec = map[string]float64{"tmdb": 1000, "bgm": 1000, "omdb": 1000, "douban": 1000}
		c.OMDbKeys, c.DoubanAPIKey = nil, ""
	}
	return &n
}

func TestScrapeSkipsLockedItem(t *testing.T) {
	m := newMediaEnv(t)
	hits := fakeProviders(t, m)
	m.ok("POST", "/api/admin/media/curate/set", "admin", map[string]any{"path": "/lib/EVA", "kind": "tv", "title": "my own title", "locked": true})

	env, _ := m.call("POST", "/api/admin/media/curate/scrape", "admin", map[string]any{"path": "/lib/EVA", "provider": "tmdb", "id": "tv/890"})
	if env.Code != 409 {
		t.Fatalf("scrape of a locked item: code %d %s", env.Code, env.Message)
	}
	if *hits != 0 {
		t.Fatalf("the provider was queried %d times for a locked item", *hits)
	}
	got, _ := db.GetVideoItemByPath("/lib/EVA")
	if got.ScrapedName != "my own title" {
		t.Fatalf("locked title overwritten: %q", got.ScrapedName)
	}
	// once unlocked it is scraped: details, ids, ratings, poster and backdrop stored as covers, episodes unlisted (no storage here)
	m.ok("POST", "/api/admin/media/curate/set", "admin", map[string]any{"path": "/lib/EVA", "locked": false})
	m.ok("POST", "/api/admin/media/curate/scrape", "admin", map[string]any{"path": "/lib/EVA", "provider": "tmdb", "id": "tv/890"})
	got, _ = db.GetVideoItemByPath("/lib/EVA")
	if got.ScrapedName == "my own title" || got.ScrapedName == "" || !strings.HasPrefix(got.Cover, "thumb:/") || !strings.HasPrefix(got.Backdrop, "thumb:/") || got.Cover == got.Backdrop {
		t.Fatalf("scrape did not fill the item: %+v", got)
	}
	rs, _ := db.ListMediaRatings(got.ID)
	if len(rs[got.ID]) == 0 || got.Rating == 0 {
		t.Fatalf("ratings were not stored: %+v rating=%v", rs, got.Rating)
	}
	if !strings.Contains(got.ExternalIDs, `"imdb":"tt0112159"`) || !strings.Contains(got.ExternalIDs, `"tmdb":"tv/890"`) {
		t.Fatalf("external ids: %s", got.ExternalIDs)
	}
	if got.Source != "manual" || got.Confidence != 1 {
		t.Fatalf("an explicit pick is manual with confidence 1: %q %v", got.Source, got.Confidence)
	}
	// scrape with force overrides a lock
	m.ok("POST", "/api/admin/media/curate/set", "admin", map[string]any{"path": "/lib/EVA", "title": "mine again", "locked": true})
	m.ok("POST", "/api/admin/media/curate/scrape", "admin", map[string]any{"path": "/lib/EVA", "provider": "tmdb", "id": "tv/890", "force": true, "lock": true})
	got, _ = db.GetVideoItemByPath("/lib/EVA")
	if got.ScrapedName == "mine again" || !got.Locked {
		t.Fatalf("forced scrape: %+v", got)
	}
}

func TestScrapeAndSearchClassifyNetworkFailure(t *testing.T) {
	m := newMediaEnv(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	mediaConfigHook = func(c *scraper.Config) {
		c.BaseURLs = map[string]string{"tmdb": deadURL + "/3", "bgm": deadURL}
		c.Timeout = 2 * time.Second
	}
	for _, ep := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/admin/media/curate/search?provider=tmdb&q=eva", nil},
		{"GET", "/api/admin/media/curate/search?provider=bgm&q=eva", nil},
		{"POST", "/api/admin/media/curate/scrape", map[string]any{"path": "/lib/EVA", "provider": "tmdb", "id": "tv/890"}},
	} {
		env, _ := m.call(ep.method, ep.path, "admin", ep.body)
		if env.Code != 502 || !strings.HasPrefix(env.Message, "network:") {
			t.Errorf("%s: code %d message %q, want 502 network:", ep.path, env.Code, env.Message)
		}
		var d struct{ Class string }
		_ = json.Unmarshal(env.Data, &d)
		if d.Class != "network" {
			t.Errorf("%s: data class %q, want network", ep.path, d.Class)
		}
		if strings.Contains(env.Message, fakeTMDBSecret) {
			t.Errorf("%s: the key is in the error message", ep.path)
		}
	}
}

func TestSearchReturnsCandidates(t *testing.T) {
	m := newMediaEnv(t)
	fakeProviders(t, m)
	raw := m.ok("GET", "/api/admin/media/curate/search?provider=tmdb&kind=tv&q="+url.QueryEscape("新世纪福音战士")+"&year=1995", "admin", nil)
	var cs []scraper.Candidate
	_ = json.Unmarshal(raw, &cs)
	if len(cs) != 1 || cs[0].ID != "tv/890" || cs[0].Provider != "tmdb" {
		t.Fatalf("candidates: %s", raw)
	}
}

func TestTMDBBaseURLSetting(t *testing.T) {
	for in, want := range map[string]string{
		"":                                      "",
		"api.themoviedb.org":                    "", // the official host is the provider default
		"https://api.themoviedb.org/":           "",
		"example.edgeone.run/api/tmdb":          "https://example.edgeone.run/api/tmdb/3",
		"https://example.edgeone.run/api/tmdb/": "https://example.edgeone.run/api/tmdb/3",
		"https://proxy.example/api/tmdb/3":      "https://proxy.example/api/tmdb/3",
		"http://10.0.0.2:8080":                  "http://10.0.0.2:8080/3",
	} {
		if got := tmdbBaseURL(in); got != want {
			t.Errorf("tmdbBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVideoScrapeStatsCountOutcomes(t *testing.T) {
	var s VideoScrapeStats
	s.record(&apply.ScrapeResult{Matched: true, Provider: "tmdb"}, 20*time.Millisecond)
	s.record(&apply.ScrapeResult{Candidates: []scraper.Candidate{{ID: "1"}}}, 10*time.Millisecond)
	s.record(&apply.ScrapeResult{}, 0)
	s.record(nil, 0) // a failed call is a miss
	got := s.snapshot()
	want := VideoScrapeStats{TotalScraped: 4, HitCount: 1, LowConfidenceCount: 1, NoMatchCount: 2, TotalSearchMillis: 30}
	if got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
}
