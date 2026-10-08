package handles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
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
		case conf.MediaTMDBToken:
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

	oldDir, oldHook, oldPriv := mediaAssetDir, mediaConfigHook, mediaAllowPrivateDownloads
	assets := t.TempDir()
	mediaAssetDir = func() string { return assets }
	mediaAllowPrivateDownloads = true
	t.Cleanup(func() { mediaAssetDir, mediaConfigHook, mediaAllowPrivateDownloads = oldDir, oldHook, oldPriv })

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
	api.POST("/fs/media", FsMedia)
	api.GET("/media/asset/:id", MediaAsset)
	api.Any("/public/settings", PublicSettings)
	ad := api.Group("/admin", middlewares.AuthAdmin)
	ad.POST("/media/set", MediaSet)
	ad.POST("/media/delete", MediaDelete)
	ad.GET("/media/list", MediaAdminList)
	ad.POST("/media/asset", MediaAssetUpload)
	ad.POST("/media/scrape", MediaScrape)
	ad.GET("/media/search", MediaSearch)
	ad.GET("/media/export", MediaExport)
	ad.POST("/media/import", MediaImport)
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

func (m *mediaEnv) upload(data []byte) uint {
	m.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "poster.png")
	_, _ = fw.Write(data)
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/api/admin/media/asset", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-User", "admin")
	w := httptest.NewRecorder()
	m.engine.ServeHTTP(w, req)
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Code != 200 {
		m.t.Fatalf("upload: %s", w.Body.String())
	}
	var out struct {
		ID uint `json:"id"`
		W  int  `json:"w"`
		H  int  `json:"h"`
	}
	_ = json.Unmarshal(env.Data, &out)
	return out.ID
}

func sp(s string) *string { return &s }

// ---- tests ----

func TestPrivateMediaSettingsNeverPublic(t *testing.T) {
	m := newMediaEnv(t)
	w := m.do("GET", "/api/public/settings", "", nil)
	body := w.Body.String()
	for _, key := range []string{conf.MediaTMDBToken, conf.MediaOMDbKeys, conf.MediaDoubanAPIKey, conf.MediaHTTPProxy} {
		if strings.Contains(body, key) {
			t.Errorf("private setting %q is in /api/public/settings", key)
		}
	}
	for _, secret := range []string{fakeTMDBSecret, fakeOMDbSecret, fakeDoubanKey, "proxy-secret-fake"} {
		if strings.Contains(body, secret) {
			t.Errorf("a secret value (%q) is in /api/public/settings", secret)
		}
	}
	for _, key := range []string{conf.MediaEnabled, conf.MediaLanguage, conf.MediaScraperTimeout} {
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
		conf.MediaEnabled: model.PUBLIC, conf.MediaTMDBToken: model.PRIVATE, conf.MediaOMDbKeys: model.PRIVATE,
		conf.MediaDoubanAPIKey: model.PRIVATE, conf.MediaLanguage: model.PUBLIC, conf.MediaHTTPProxy: model.PRIVATE,
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
			{"POST", "/api/admin/media/set"}, {"POST", "/api/admin/media/delete"}, {"GET", "/api/admin/media/list"},
			{"POST", "/api/admin/media/asset"}, {"POST", "/api/admin/media/scrape"}, {"GET", "/api/admin/media/search?q=x"},
			{"GET", "/api/admin/media/export"}, {"POST", "/api/admin/media/import"},
		} {
			env, _ := m.call(ep.method, ep.path, user, map[string]any{"path": "/x", "kind": "movie"})
			if env.Code == 200 {
				t.Errorf("%s %s answered 200 for user %q", ep.method, ep.path, user)
			}
		}
	}
	if it, _ := db.GetMediaItemByPath("/x"); it != nil {
		t.Fatal("a non-admin created an item")
	}
}

func TestSetOverwritesOnlyGivenFields(t *testing.T) {
	m := newMediaEnv(t)
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{
		"path": "/lib/Show", "kind": "tv", "title": "Show", "year": 2001, "overview": "keep me",
		"external_ids": map[string]string{"tmdb": "tv/1"}, "genres": []string{"a"},
	})
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib/Show", "title": "Show 2", "locked": true})
	got, _ := db.GetMediaItemByPath("/lib/Show")
	if got.Title != "Show 2" || !got.Locked {
		t.Fatalf("given fields: %+v", got)
	}
	if got.Kind != "tv" || got.Year != 2001 || got.Overview != "keep me" || got.ExternalIDs != `{"tmdb":"tv/1"}` || got.Genres != `["a"]` {
		t.Fatalf("fields not in the request changed: %+v", got)
	}
	env, _ := m.call("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib/Show", "poster_id": 999})
	if env.Code == 200 {
		t.Fatal("a poster_id that is not an asset was accepted")
	}
}

func TestFsMediaListDTOHasNoHeavyFields(t *testing.T) {
	m := newMediaEnv(t)
	poster := m.upload(testPNG(t, 400, 600, color.RGBA{9, 9, 200, 255}))
	for _, p := range []string{"/lib/A", "/lib/B"} {
		m.ok("POST", "/api/admin/media/set", "admin", map[string]any{
			"path": p, "kind": "movie", "title": "T " + p, "year": 2000, "overview": "LONG OVERVIEW " + p,
			"ratings": map[string]any{"tmdb": 8.1}, "poster_id": poster,
			"candidates": []map[string]any{{"provider": "tmdb", "id": "movie/1", "title": "c", "year": 2000, "score": 0.5}},
		})
	}
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib", "kind": "collection", "title": "Lib", "overview": "collection text",
		"episodes": []map[string]any{{"file_name": "x.mkv", "season": 1, "episode": 1, "title": "ep"}}})
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib/A/deep", "kind": "movie", "title": "deeper"})

	raw := m.ok("POST", "/api/fs/media", "alice", map[string]any{"path": "/lib"})
	var generic struct {
		Item     map[string]any   `json:"item"`
		Children []map[string]any `json:"children"`
		Episodes []map[string]any `json:"episodes"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if len(generic.Children) != 2 {
		t.Fatalf("children = %d, want the 2 direct ones: %s", len(generic.Children), raw)
	}
	for _, ch := range generic.Children {
		for _, banned := range []string{"overview", "candidates", "episodes", "external_ids", "genres"} {
			if _, has := ch[banned]; has {
				t.Errorf("list DTO carries %q: %v", banned, ch)
			}
		}
		for _, need := range []string{"path", "kind", "title", "year", "ratings", "poster_url"} {
			if _, has := ch[need]; !has {
				t.Errorf("list DTO lacks %q", need)
			}
		}
		if u, _ := ch["poster_url"].(string); !strings.Contains(u, "/api/media/asset/") || !strings.Contains(u, "v=thumb") || !strings.Contains(u, "sign=") {
			t.Errorf("poster_url = %q", u)
		}
	}
	if generic.Item == nil || generic.Item["overview"] != "collection text" {
		t.Fatalf("detail item: %v", generic.Item)
	}
	if len(generic.Episodes) != 1 {
		t.Fatalf("episodes = %v", generic.Episodes)
	}
	if strings.Contains(string(raw), "LONG OVERVIEW") {
		t.Fatal("a child's overview leaked into the response")
	}
	// the detail of a child carries everything
	rawA := m.ok("POST", "/api/fs/media", "admin", map[string]any{"path": "/lib/A"})
	if !strings.Contains(string(rawA), "LONG OVERVIEW /lib/A") || !strings.Contains(string(rawA), `"candidates":[{`) {
		t.Fatalf("detail incomplete: %s", rawA)
	}
	// a non-admin does not see the candidates
	rawAlice := m.ok("POST", "/api/fs/media", "alice", map[string]any{"path": "/lib/A"})
	if strings.Contains(string(rawAlice), `"provider"`) {
		t.Fatalf("candidates visible to a non-admin: %s", rawAlice)
	}
}

func TestFsMediaRespectsPermissionsAndSwitch(t *testing.T) {
	m := newMediaEnv(t)
	for _, p := range []string{"/secret", "/secret/Movie", "/open/Movie", "/open/.hidden"} {
		m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": p, "kind": "movie", "title": "T" + p})
	}
	if err := op.CreateMeta(&model.Meta{Path: "/secret", Password: "pw", PSub: true}); err != nil {
		t.Fatal(err)
	}
	if err := op.CreateMeta(&model.Meta{Path: "/open", Hide: `^\.hidden$`, HSub: true}); err != nil {
		t.Fatal(err)
	}

	count := func(raw json.RawMessage) (item bool, kids int) {
		var r struct {
			Item     *json.RawMessage  `json:"item"`
			Children []json.RawMessage `json:"children"`
		}
		_ = json.Unmarshal(raw, &r)
		return r.Item != nil, len(r.Children)
	}

	// no password: empty, not an error
	env, _ := m.call("POST", "/api/fs/media", "alice", map[string]any{"path": "/secret"})
	if env.Code != 200 {
		t.Fatalf("a path without access must answer empty, got code %d", env.Code)
	}
	if item, kids := count(env.Data); item || kids != 0 {
		t.Fatalf("no-permission path leaked: %s", env.Data)
	}
	// wrong password too
	env, _ = m.call("POST", "/api/fs/media", "alice", map[string]any{"path": "/secret", "password": "nope"})
	if item, kids := count(env.Data); item || kids != 0 {
		t.Fatalf("wrong password leaked: %s", env.Data)
	}
	// right password
	env, _ = m.call("POST", "/api/fs/media", "alice", map[string]any{"path": "/secret", "password": "pw"})
	if item, kids := count(env.Data); !item || kids != 1 {
		t.Fatalf("right password: %s", env.Data)
	}
	// the hide rule removes the hidden child for a user who can't see hides, not for the admin
	env, _ = m.call("POST", "/api/fs/media", "alice", map[string]any{"path": "/open"})
	if _, kids := count(env.Data); kids != 1 {
		t.Fatalf("hidden child visible: %s", env.Data)
	}
	env, _ = m.call("POST", "/api/fs/media", "admin", map[string]any{"path": "/open"})
	if _, kids := count(env.Data); kids != 2 {
		t.Fatalf("admin should see both: %s", env.Data)
	}
	// a user with a base path sees paths relative to it, and cannot climb out
	env, _ = m.call("POST", "/api/fs/media", "bob", map[string]any{"path": "/../secret"})
	if item, kids := count(env.Data); item || kids != 0 {
		t.Fatalf("path escape leaked: %s", env.Data)
	}
	// guest disabled -> the same 401 the listing gives is not required here; media off -> empty
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib/X", "kind": "movie", "title": "x"})
	_ = op.SaveSettingItem(&model.SettingItem{Key: conf.MediaEnabled, Value: "false", Type: conf.TypeBool, Group: model.MEDIA})
	env, _ = m.call("POST", "/api/fs/media", "admin", map[string]any{"path": "/lib"})
	if item, kids := count(env.Data); item || kids != 0 {
		t.Fatalf("media_enabled=false still answers: %s", env.Data)
	}
}

func TestAssetSignatureAndCaching(t *testing.T) {
	m := newMediaEnv(t)
	pngData := testPNG(t, 700, 1000, color.RGBA{200, 0, 0, 255})
	id := m.upload(pngData)
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib/P", "kind": "movie", "poster_id": id})

	raw := m.ok("POST", "/api/fs/media", "alice", map[string]any{"path": "/lib/P"})
	var r struct {
		Item struct {
			PosterURL string `json:"poster_url"`
		} `json:"item"`
	}
	_ = json.Unmarshal(raw, &r)
	good := r.Item.PosterURL
	if good == "" {
		t.Fatalf("no poster url: %s", raw)
	}

	// valid signature, no Authorization header at all
	w := m.do("GET", good, "", nil)
	if w.Code != 200 {
		t.Fatalf("signed url: HTTP %d %s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", cc)
	}
	etag := w.Header().Get("ETag")
	if etag == "" || !strings.Contains(etag, "") || w.Header().Get("Content-Type") != "image/png" {
		t.Errorf("ETag %q, type %q", etag, w.Header().Get("Content-Type"))
	}
	if !bytes.Equal(w.Body.Bytes(), pngData) {
		t.Error("original bytes differ")
	}
	// conditional request
	req := httptest.NewRequest("GET", good, nil)
	req.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	m.engine.ServeHTTP(w2, req)
	if w2.Code != http.StatusNotFound && w2.Code != http.StatusNotModified {
		t.Errorf("If-None-Match -> HTTP %d", w2.Code)
	}
	if w2.Code != http.StatusNotModified {
		t.Errorf("want 304, got %d", w2.Code)
	}

	// thumbnail variant is a narrower JPEG
	u, _ := url.Parse(good)
	q := u.Query()
	q.Set("v", "thumb")
	wt := m.do("GET", u.Path+"?"+q.Encode(), "", nil)
	if wt.Code != 200 || wt.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb: HTTP %d %s", wt.Code, wt.Header().Get("Content-Type"))
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(wt.Body.Bytes()))
	if err != nil || cfg.Width != 342 {
		t.Errorf("thumb width = %d (%v), want 342", cfg.Width, err)
	}

	// forged, missing, or someone else's signature: 403
	bad := []string{
		u.Path + "?v=thumb&sign=forged",
		u.Path + "?v=thumb",
		strings.Replace(good, fmt.Sprintf("/asset/%d?", id), fmt.Sprintf("/asset/%d?", id+1), 1),
	}
	for _, b := range bad {
		if w := m.do("GET", b, "", nil); w.Code != http.StatusForbidden {
			t.Errorf("GET %s -> HTTP %d, want 403", b, w.Code)
		}
	}
}

func TestAssetDedupBySHAOverHTTP(t *testing.T) {
	m := newMediaEnv(t)
	m.noProxy()
	a := testPNG(t, 100, 150, color.RGBA{1, 2, 3, 255})
	id1, id2 := m.upload(a), m.upload(a)
	id3 := m.upload(testPNG(t, 100, 150, color.RGBA{4, 5, 6, 255}))
	if id1 != id2 || id3 == id1 {
		t.Fatalf("ids %d %d %d: same bytes must share, different bytes must not", id1, id2, id3)
	}
	// url form: the server downloads it, through the same dedup
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(a)
	}))
	defer img.Close()
	var out struct{ ID uint }
	raw := m.ok("POST", "/api/admin/media/asset", "admin", map[string]any{"url": img.URL + "/p.png"})
	_ = json.Unmarshal(raw, &out)
	if out.ID != id1 {
		t.Fatalf("download of the same image got asset %d, want %d", out.ID, id1)
	}
	if env, _ := m.call("POST", "/api/admin/media/asset", "admin", map[string]any{"url": "file:///etc/passwd"}); env.Code == 200 {
		t.Fatal("a file:// URL was accepted")
	}
}

func TestDeleteAndPendingList(t *testing.T) {
	m := newMediaEnv(t)
	set := func(p string, conf float64, locked bool) {
		m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": p, "kind": "tv", "confidence": conf, "locked": locked})
	}
	set("/d/a", 0.3, false)
	set("/d/b", 0.9, false)
	set("/d/c", 0.2, true)
	set("/d/a/s1", 0.9, false)
	set("/e/z", 0.1, false)

	raw := m.ok("GET", "/api/admin/media/list?prefix=/d&pending=true", "admin", nil)
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
	raw = m.ok("GET", "/api/admin/media/list?kind=tv&per_page=2&page=2", "admin", nil)
	_ = json.Unmarshal(raw, &l)
	if l.Total != 5 || len(l.Content) != 2 {
		t.Fatalf("paging: %s", raw)
	}

	if env, _ := m.call("POST", "/api/admin/media/delete", "admin", map[string]any{"prefix": "/", "recursive": true}); env.Code == 200 {
		t.Fatal("deleting everything must be refused")
	}
	var del struct{ Deleted int }
	_ = json.Unmarshal(m.ok("POST", "/api/admin/media/delete", "admin", map[string]any{"prefix": "/d", "recursive": true}), &del)
	if del.Deleted != 4 {
		t.Fatalf("deleted %d, want 4", del.Deleted)
	}
	if it, _ := db.GetMediaItemByPath("/e/z"); it == nil {
		t.Fatal("recursive delete went outside the prefix")
	}
	_ = json.Unmarshal(m.ok("POST", "/api/admin/media/delete", "admin", map[string]any{"path": "/e/z"}), &del)
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
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib/EVA", "kind": "tv", "title": "my own title", "locked": true})

	env, _ := m.call("POST", "/api/admin/media/scrape", "admin", map[string]any{"path": "/lib/EVA", "provider": "tmdb", "id": "tv/890"})
	if env.Code != 409 {
		t.Fatalf("scrape of a locked item: code %d %s", env.Code, env.Message)
	}
	if *hits != 0 {
		t.Fatalf("the provider was queried %d times for a locked item", *hits)
	}
	got, _ := db.GetMediaItemByPath("/lib/EVA")
	if got.Title != "my own title" {
		t.Fatalf("locked title overwritten: %q", got.Title)
	}
	// once unlocked it is scraped: details, ids, ratings, poster stored as an asset, episodes unlisted (no storage here)
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib/EVA", "locked": false})
	m.ok("POST", "/api/admin/media/scrape", "admin", map[string]any{"path": "/lib/EVA", "provider": "tmdb", "id": "tv/890"})
	got, _ = db.GetMediaItemByPath("/lib/EVA")
	if got.Title == "my own title" || got.Title == "" || got.PosterID == 0 || got.BackdropID == 0 {
		t.Fatalf("scrape did not fill the item: %+v", got)
	}
	if !strings.Contains(got.ExternalIDs, `"imdb":"tt0112159"`) || !strings.Contains(got.ExternalIDs, `"tmdb":"tv/890"`) {
		t.Fatalf("external ids: %s", got.ExternalIDs)
	}
	if got.Source != "manual" || got.Confidence != 1 {
		t.Fatalf("an explicit pick is manual with confidence 1: %q %v", got.Source, got.Confidence)
	}
	// scrape with force overrides a lock
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib/EVA", "title": "mine again", "locked": true})
	m.ok("POST", "/api/admin/media/scrape", "admin", map[string]any{"path": "/lib/EVA", "provider": "tmdb", "id": "tv/890", "force": true, "lock": true})
	got, _ = db.GetMediaItemByPath("/lib/EVA")
	if got.Title == "mine again" || !got.Locked {
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
		{"GET", "/api/admin/media/search?provider=tmdb&q=eva", nil},
		{"GET", "/api/admin/media/search?provider=bgm&q=eva", nil},
		{"POST", "/api/admin/media/scrape", map[string]any{"path": "/lib/EVA", "provider": "tmdb", "id": "tv/890"}},
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
	raw := m.ok("GET", "/api/admin/media/search?provider=tmdb&kind=tv&q="+url.QueryEscape("新世纪福音战士")+"&year=1995", "admin", nil)
	var cs []scraper.Candidate
	_ = json.Unmarshal(raw, &cs)
	if len(cs) != 1 || cs[0].ID != "tv/890" || cs[0].Provider != "tmdb" {
		t.Fatalf("candidates: %s", raw)
	}
}

func TestExportImportRoundTripRedownloadsAssets(t *testing.T) {
	m := newMediaEnv(t)
	m.noProxy()
	imgData := testPNG(t, 300, 450, color.RGBA{50, 60, 70, 255})
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgData)
	}))
	defer img.Close()

	// a poster known by its source URL
	var asset struct{ ID uint }
	_ = json.Unmarshal(m.ok("POST", "/api/admin/media/asset", "admin", map[string]any{"url": img.URL + "/poster.png"}), &asset)
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{
		"path": "/lib/Exp", "kind": "tv", "title": "Exp", "overview": "o", "poster_id": asset.ID, "locked": true,
		"external_ids": map[string]string{"tmdb": "tv/1"}, "ratings": map[string]any{"tmdb": 7.5},
		"episodes": []map[string]any{{"file_name": "S01E01.mkv", "season": 1, "episode": 1, "title": "one"}},
	})
	w := m.do("GET", "/api/admin/media/export", "admin", nil)
	if w.Code != 200 {
		t.Fatalf("export: %d", w.Code)
	}
	doc := w.Body.Bytes()
	if strings.Contains(string(doc), "base64") || bytes.Contains(doc, imgData[:16]) {
		t.Fatal("export embeds image bytes")
	}
	var parsed struct {
		Items []struct {
			Path      string `json:"path"`
			PosterSHA string `json:"poster_sha"`
		} `json:"items"`
		Assets []struct {
			SHA256    string `json:"sha256"`
			SourceURL string `json:"source_url"`
		} `json:"assets"`
	}
	_ = json.Unmarshal(doc, &parsed)
	if len(parsed.Items) != 1 || parsed.Items[0].PosterSHA == "" || len(parsed.Assets) != 1 || !strings.HasSuffix(parsed.Assets[0].SourceURL, "/poster.png") {
		t.Fatalf("export document: %s", doc)
	}

	// wipe the asset row and file, drop the item, then import
	_, _ = db.DeleteMediaItemByPath("/lib/Exp")
	if err := db.GetDb().Where("1 = 1").Delete(&model.MediaAsset{}).Error; err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(mediaAssetDir())

	var res struct {
		Imported   int `json:"imported"`
		Downloaded int `json:"assets_downloaded"`
		Missing    int `json:"assets_missing"`
	}
	req := httptest.NewRequest("POST", "/api/admin/media/import", bytes.NewReader(doc))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User", "admin")
	rec := httptest.NewRecorder()
	m.engine.ServeHTTP(rec, req)
	var env envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Code != 200 {
		t.Fatalf("import: %s", rec.Body.String())
	}
	_ = json.Unmarshal(env.Data, &res)
	if res.Imported != 1 || res.Downloaded != 1 || res.Missing != 0 {
		t.Fatalf("import result: %+v", res)
	}
	got, _ := db.GetMediaItemByPath("/lib/Exp")
	if got == nil || got.PosterID == 0 || !got.Locked || got.Title != "Exp" || got.ExternalIDs != `{"tmdb":"tv/1"}` {
		t.Fatalf("imported item: %+v", got)
	}
	if eps, _ := db.GetMediaEpisodes(got.ID); len(eps) != 1 {
		t.Fatalf("episodes after import: %d", len(eps))
	}
	// importing again leaves a locked item alone
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib/Exp", "title": "edited after"})
	req = httptest.NewRequest("POST", "/api/admin/media/import", bytes.NewReader(doc))
	req.Header.Set("X-User", "admin")
	rec = httptest.NewRecorder()
	m.engine.ServeHTTP(rec, req)
	got, _ = db.GetMediaItemByPath("/lib/Exp")
	if got.Title != "edited after" {
		t.Fatalf("import overwrote a locked item: %q", got.Title)
	}
}

func TestLibraryRootOpensWithItsChildren(t *testing.T) {
	m := newMediaEnv(t)
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib", "kind": "library", "title": "Lib", "locked": true, "source": "manual"})
	m.ok("POST", "/api/admin/media/set", "admin", map[string]any{"path": "/lib/A", "kind": "movie", "title": "A"})
	raw := m.ok("POST", "/api/fs/media", "alice", map[string]any{"path": "/lib"})
	var got struct {
		Item struct {
			Kind string `json:"kind"`
		} `json:"item"`
		Children []struct {
			Path string `json:"path"`
		} `json:"children"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Item.Kind != "library" || len(got.Children) != 1 || got.Children[0].Path != "/lib/A" {
		t.Fatalf("library response: %s", raw)
	}
	if !strings.Contains(string(m.ok("GET", "/api/admin/media/list?kind=library", "admin", nil)), "/lib") {
		t.Fatal("admin list by kind=library lost the root")
	}
}
