package handles

import (
	"encoding/json"
	"image/color"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
)

// coverEnv is a media env with the library switched on and an imgs directory.
func coverEnv(t *testing.T) *mediaEnv {
	t.Helper()
	m := newMediaEnv(t)
	setBool(t, conf.MediaEnabled, true)
	setBool(t, conf.MediaProxyCover, true)
	for _, mt := range []model.MediaType{model.MediaTypeVideo, model.MediaTypeMusic} {
		if err := db.SaveMediaConfig(&model.MediaConfig{MediaType: mt, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(conf.DataDir, "imgs"), 0o755); err != nil {
		t.Fatal(err)
	}
	return m
}

func setBool(t *testing.T, key string, v bool) {
	t.Helper()
	it := &model.SettingItem{Key: key, Value: strconv.FormatBool(v), Type: conf.TypeBool, Group: model.MEDIA, Flag: model.PRIVATE}
	if err := op.SaveSettingItem(it); err != nil {
		t.Fatal(err)
	}
}

func smallPNG(t *testing.T) []byte { return testPNG(t, 4, 4, color.RGBA{9, 9, 9, 255}) }

// writeThumb puts an image in the runtime thumbnail directory and returns the
// cover reference the scraper would store for it.
func writeThumb(t *testing.T, name string, body []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(conf.DataDir, "imgs", name), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return "thumb:/imgs/" + name
}

func saveVideo(t *testing.T, folder, name, cover, backdrop string) *model.MediaItem {
	t.Helper()
	it := &model.MediaItem{MediaType: model.MediaTypeVideo, FolderPath: folder, FileName: name, ScrapedName: name, Cover: cover}
	it.Backdrop = backdrop
	if err := db.SaveVideoItem(it, nil, nil); err != nil {
		t.Fatal(err)
	}
	return it
}

func (m *mediaEnv) get(path, user string) *httptest.ResponseRecorder {
	return m.do("GET", path, user, nil)
}

type listedItem struct {
	FileName    string `json:"file_name"`
	Cover       string `json:"cover"`
	CoverURL    string `json:"cover_url"`
	BackdropURL string `json:"backdrop_url"`
}

func (m *mediaEnv) listVideos(user string) ([]listedItem, string) {
	m.t.Helper()
	w := m.get("/api/fs/media/list?media_type=video", user)
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Code != 200 {
		m.t.Fatalf("list: HTTP %d %s", w.Code, w.Body.String())
	}
	var page struct{ Content []listedItem }
	if err := json.Unmarshal(env.Data, &page); err != nil {
		m.t.Fatal(err)
	}
	return page.Content, w.Body.String()
}

func TestThumbNeedsAValidSignature(t *testing.T) {
	m := coverEnv(t)
	png := smallPNG(t)
	writeThumb(t, "a.jpg", png)
	writeThumb(t, "b.jpg", png)
	q := func(path, sig string) string {
		u := "/api/public/media/thumb?path=" + url.QueryEscape(path)
		if sig != "" {
			u += "&sign=" + url.QueryEscape(sig)
		}
		return u
	}
	good := sign.Sign(mediaThumbSignPrefix + "/imgs/a.jpg")
	for name, tc := range map[string]struct {
		url  string
		want int
	}{
		"no sign":                     {q("/imgs/a.jpg", ""), 403},
		"garbage sign":                {q("/imgs/a.jpg", "AAAA:0"), 403},
		"unprefixed signature":        {q("/imgs/a.jpg", sign.Sign("/imgs/a.jpg")), 403},
		"proxy signature on thumb":    {q("/imgs/a.jpg", sign.Sign(mediaProxySignPrefix+"/imgs/a.jpg")), 403},
		"signature of another file":   {q("/imgs/b.jpg", good), 403},
		"expired signature":           {q("/imgs/a.jpg", sign.WithDuration(mediaThumbSignPrefix+"/imgs/a.jpg", -time.Minute)), 403},
		"valid signature":             {q("/imgs/a.jpg", good), 200},
		"valid, path spelled with ..": {q("/imgs/x/../a.jpg", good), 200},
	} {
		if w := m.get(tc.url, ""); w.Code != tc.want {
			t.Errorf("%s: HTTP %d, want %d", name, w.Code, tc.want)
		}
	}
}

func TestProxyNeedsAValidSignature(t *testing.T) {
	m := coverEnv(t)
	const img = "https://image.tmdb.org/t/p/w500/x.jpg"
	q := func(sig string) string {
		u := "/api/public/media/proxy?url=" + url.QueryEscape(img)
		if sig != "" {
			u += "&sign=" + url.QueryEscape(sig)
		}
		return u
	}
	for name, sig := range map[string]string{
		"no sign":                  "",
		"garbage sign":             "AAAA:0",
		"thumb signature on proxy": sign.Sign(mediaThumbSignPrefix + img),
		"unprefixed signature":     sign.Sign(img),
		"signature of another url": sign.Sign(mediaProxySignPrefix + "https://image.tmdb.org/t/p/w500/y.jpg"),
		"expired signature":        sign.WithDuration(mediaProxySignPrefix+img, -time.Minute),
	} {
		if w := m.get(q(sig), ""); w.Code != 403 {
			t.Errorf("%s: HTTP %d, want 403", name, w.Code)
		}
	}
	// A valid signature only opens the gate: a host off the list is still refused.
	off := "https://evil.example/x.jpg"
	u := "/api/public/media/proxy?url=" + url.QueryEscape(off) + "&sign=" + url.QueryEscape(sign.Sign(mediaProxySignPrefix+off))
	if w := m.get(u, ""); w.Code != 403 {
		t.Errorf("signed url of a host off the list: HTTP %d, want 403", w.Code)
	}
	// With neither url nor signature the answer is 403, not a hint about parameters.
	if w := m.get("/api/public/media/proxy", ""); w.Code != 403 {
		t.Errorf("bare proxy request: HTTP %d, want 403", w.Code)
	}
}

func TestListCarriesSignedCoverURLs(t *testing.T) {
	m := coverEnv(t)
	png := smallPNG(t)
	saveVideo(t, "/lib", "thumb.mkv", writeThumb(t, "t.jpg", png), writeThumb(t, "tb.jpg", png))
	saveVideo(t, "/lib", "remote.mkv", "https://image.tmdb.org/t/p/w500/r.jpg", "")
	saveVideo(t, "/lib", "local.mkv", "/d/imgs/l.jpg?sign=stale-sign", "")
	saveVideo(t, "/lib", "inline.mkv", "data:image/png;base64,AAAA", "")
	saveVideo(t, "/lib", "none.mkv", "", "")

	items, _ := m.listVideos("admin")
	by := map[string]listedItem{}
	for _, it := range items {
		by[it.FileName] = it
	}
	if len(by) != 5 {
		t.Fatalf("listed %d items, want 5", len(by))
	}

	th := by["thumb.mkv"]
	if !strings.HasPrefix(th.CoverURL, "/api/public/media/thumb?") || !strings.HasPrefix(th.BackdropURL, "/api/public/media/thumb?") || th.CoverURL == th.BackdropURL {
		t.Fatalf("thumb urls: cover=%q backdrop=%q", th.CoverURL, th.BackdropURL)
	}
	for _, u := range []string{th.CoverURL, th.BackdropURL} {
		if w := m.get(u, ""); w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("fetching %s: HTTP %d", strings.SplitN(u, "&sign=", 2)[0], w.Code)
		}
	}

	rm := by["remote.mkv"].CoverURL
	pu, err := url.Parse(rm)
	if err != nil || pu.Path != "/api/public/media/proxy" || pu.Query().Get("url") != "https://image.tmdb.org/t/p/w500/r.jpg" {
		t.Fatalf("remote cover_url = %q", rm)
	}
	if err := sign.Verify(mediaProxySignPrefix+pu.Query().Get("url"), pu.Query().Get("sign")); err != nil {
		t.Errorf("proxy url signature: %v", err)
	}

	lu, err := url.Parse(by["local.mkv"].CoverURL)
	if err != nil || lu.Path != "/d/imgs/l.jpg" || lu.Query().Get("sign") == "stale-sign" || lu.Query().Get("sign") == "" {
		t.Fatalf("local cover_url = %q", by["local.mkv"].CoverURL)
	}
	if err := sign.Verify("/imgs/l.jpg", lu.Query().Get("sign")); err != nil {
		t.Errorf("/d signature: %v", err)
	}

	if got := by["inline.mkv"].CoverURL; got != "data:image/png;base64,AAAA" {
		t.Errorf("base64 cover_url = %q, want the data itself", got)
	}
	if n := by["none.mkv"]; n.CoverURL != "" || n.BackdropURL != "" {
		t.Errorf("an item without images has urls: %+v", n)
	}

	// without proxy_cover a remote url is handed out as it is
	setBool(t, conf.MediaProxyCover, false)
	items, _ = m.listVideos("admin")
	for _, it := range items {
		if it.FileName == "remote.mkv" && it.CoverURL != "https://image.tmdb.org/t/p/w500/r.jpg" {
			t.Errorf("remote cover_url without proxy = %q", it.CoverURL)
		}
	}
}

func TestItemDetailCarriesCoverURL(t *testing.T) {
	m := coverEnv(t)
	it := saveVideo(t, "/lib", "one.mkv", writeThumb(t, "one.jpg", smallPNG(t)), "")
	w := m.get("/api/fs/media/item/"+strconv.Itoa(int(it.ID)), "admin")
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	var got listedItem
	_ = json.Unmarshal(env.Data, &got)
	if env.Code != 200 || !strings.HasPrefix(got.CoverURL, "/api/public/media/thumb?") {
		t.Fatalf("detail: HTTP %d code %d cover_url=%q", w.Code, env.Code, got.CoverURL)
	}
}

func TestAlbumsCarryCoverURL(t *testing.T) {
	m := coverEnv(t)
	cover := writeThumb(t, "al.jpg", smallPNG(t))
	if err := db.CreateOrUpdateMediaItem(&model.MediaItem{MediaType: model.MediaTypeMusic, FolderPath: "/music", FileName: "1.flac", AlbumName: "A", AlbumArtist: "Z", Cover: cover}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/fs/media/albums", "/api/fs/media/album?album_name=A&album_artist=Z"} {
		w := m.get(path, "admin")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"cover_url":"/api/public/media/thumb?`) {
			t.Errorf("%s: HTTP %d %s", path, w.Code, w.Body.String())
		}
	}
}

// An item a user may not see must not leak its cover, neither as a url nor as
// the stored reference.
func TestInvisibleItemHasNoCoverURL(t *testing.T) {
	m := coverEnv(t)
	png := smallPNG(t)
	saveVideo(t, "/lib", "seen.mkv", writeThumb(t, "seen.jpg", png), "")
	hid := saveVideo(t, "/secret", "hidden.mkv", writeThumb(t, "secret-cover.jpg", png), writeThumb(t, "secret-back.jpg", png))

	// bob's base path is /lib
	items, body := m.listVideos("bob")
	if len(items) != 1 || items[0].FileName != "seen.mkv" {
		t.Fatalf("bob sees %+v", items)
	}
	if strings.Contains(body, "secret") {
		t.Errorf("the list for bob mentions the invisible item: %s", body)
	}
	w := m.get("/api/fs/media/item/"+strconv.Itoa(int(hid.ID)), "bob")
	var env envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Code != 404 || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "cover_url") {
		t.Errorf("detail of an invisible item: code %d body %s", env.Code, w.Body.String())
	}
}
