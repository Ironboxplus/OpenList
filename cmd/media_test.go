package cmd

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	mm "github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/spf13/cobra"
)

// fakeOP answers like the OpenList endpoints the media commands use: a
// directory tree for /api/fs/list, and an in-memory media store behind the
// admin endpoints. Every write is recorded.
type fakeOP struct {
	t       *testing.T
	mu      sync.Mutex
	tree    map[string][]map[string]any
	items   map[string]mm.MediaSetReq
	locked  map[string]bool
	sets    []mm.MediaSetReq
	assets  map[string]int // sha-ish key -> id
	lists   []string
	uploads int
	scrapes []map[string]any
	ids     map[string]map[string]string // path -> external ids, for /api/fs/media
	rawURL  string                       // what /api/fs/get answers
	gets    []string
}

func newFakeOP(t *testing.T) (*fakeOP, *httptest.Server) {
	f := &fakeOP{t: t, tree: map[string][]map[string]any{}, items: map[string]mm.MediaSetReq{}, locked: map[string]bool{}, assets: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeOP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(code int, msg string, data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg, "data": data})
	}
	if r.Header.Get("Authorization") != "file-token" && r.Header.Get("Authorization") != "tok" {
		reply(401, "unauthorized", nil)
		return
	}
	switch r.URL.Path {
	case "/api/fs/list":
		var req struct{ Path string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.lists = append(f.lists, req.Path)
		ents, ok := f.tree[req.Path]
		if !ok {
			reply(500, "failed get objs: not a folder", nil)
			return
		}
		reply(200, "success", map[string]any{"content": ents, "total": len(ents)})
	case "/api/admin/media/list":
		prefix := r.URL.Query().Get("prefix")
		var out []map[string]any
		for p, it := range f.items {
			if strings.HasPrefix(p, prefix) && (r.URL.Query().Get("kind") == "" || r.URL.Query().Get("kind") == *it.Kind) {
				out = append(out, map[string]any{"path": p, "kind": *it.Kind, "locked": f.locked[p], "confidence": 1})
			}
		}
		reply(200, "success", map[string]any{"content": out, "total": len(out)})
	case "/api/admin/media/set":
		var req mm.MediaSetReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.sets = append(f.sets, req)
		f.items[req.Path] = req
		if req.Locked != nil {
			f.locked[req.Path] = *req.Locked
		}
		reply(200, "success", nil)
	case "/api/admin/media/asset":
		f.uploads++
		file, _, err := r.FormFile("file")
		if err != nil {
			reply(400, err.Error(), nil)
			return
		}
		b, _ := io.ReadAll(file)
		key := string(b)
		id, ok := f.assets[key]
		if !ok {
			id = len(f.assets) + 1
			f.assets[key] = id
		}
		reply(200, "success", map[string]any{"id": id, "w": 10, "h": 10})
	case "/api/admin/media/search":
		q := r.URL.Query().Get("q")
		if q == "盗梦空间" {
			reply(200, "success", []scraper.Candidate{{Provider: "tmdb", ID: "movie/27205", Title: "盗梦空间", Year: 2010, Score: 0.92}})
			return
		}
		reply(200, "success", []scraper.Candidate{{Provider: "tmdb", ID: "tv/1", Title: "Something", Year: 2001, Score: 0.31}, {Provider: "tmdb", ID: "tv/2", Title: "Other", Year: 2002, Score: 0.3}})
	case "/api/admin/media/delete":
		var req struct{ Path string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		delete(f.items, req.Path)
		delete(f.locked, req.Path)
		reply(200, "success", nil)
	case "/api/fs/media":
		var req struct{ Path string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply(200, "success", map[string]any{"item": map[string]any{"path": req.Path, "external_ids": f.ids[req.Path]}})
	case "/api/fs/get":
		var req struct{ Path string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.gets = append(f.gets, req.Path)
		reply(200, "success", map[string]any{"raw_url": f.rawURL})
	case "/api/admin/media/scrape":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.scrapes = append(f.scrapes, body)
		reply(200, "success", map[string]any{"path": body["path"], "matched": true})
	default:
		reply(404, "no such endpoint "+r.URL.Path, nil)
	}
}

func (f *fakeOP) dir(path string, entries ...map[string]any) { f.tree[path] = entries }
func d(name string) map[string]any                           { return map[string]any{"name": name, "is_dir": true, "size": 0} }
func v(name string) map[string]any {
	return map[string]any{"name": name, "is_dir": false, "size": int64(8 << 30)}
}

func pointAtFakeOP(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old, oldM := syncFlags, mediaFlags
	syncFlags.server, syncFlags.token = srv.URL, "tok"
	mediaFlags = oldM
	t.Cleanup(func() { syncFlags, mediaFlags = old, oldM })
}

func run(t *testing.T, fn func(*cobra.Command, []string) error, args ...string) (string, error) {
	t.Helper()
	c := &cobra.Command{}
	var out bytes.Buffer
	c.SetOut(&out)
	err := fn(c, args)
	return out.String(), err
}

func TestPlanEditApplyAgainstAFakeServer(t *testing.T) {
	f, srv := newFakeOP(t)
	pointAtFakeOP(t, srv)
	root := "/115/云下载"
	f.dir(root, d("盗梦空间"), d("某种物质"), d("行尸走肉"))
	f.dir(root+"/盗梦空间", v("Inception.2010.mkv"))
	f.dir(root+"/某种物质", v("The.Substance.mkv"))
	f.dir(root+"/行尸走肉", d("Season 1"), d("Season 2"))
	f.items[root+"/行尸走肉"] = mm.MediaSetReq{Path: root + "/行尸走肉", Kind: ptr("tv")}
	f.locked[root+"/行尸走肉"] = true

	// plan: nothing is written
	mediaFlags.root, mediaFlags.depth = root, 1
	tsv, err := run(t, runMediaPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sets) != 0 || f.uploads != 0 {
		t.Fatal("plan wrote to the server")
	}
	for _, l := range f.lists {
		if strings.Contains(l, "行尸走肉") {
			t.Fatalf("a locked directory was listed: %v", f.lists)
		}
	}
	lines := strings.Split(strings.TrimSpace(tsv), "\n")
	if !strings.HasPrefix(lines[0], "#path\tkind\tprovider\tid\ttitle\tyear\tconfidence\tnote") || len(lines) != 3 {
		t.Fatalf("plan output:\n%s", tsv)
	}
	var inception, substance string
	for _, l := range lines[1:] {
		cols := strings.Split(l, "\t")
		if len(cols) != 8 {
			t.Fatalf("row has %d columns: %q", len(cols), l)
		}
		switch {
		case strings.HasSuffix(cols[0], "盗梦空间"):
			inception = l
			if cols[1] != "movie" || cols[2] != "tmdb" || cols[3] != "movie/27205" {
				t.Fatalf("inception row: %q", l)
			}
		case strings.HasSuffix(cols[0], "某种物质"):
			substance = l
			if !strings.Contains(cols[7], "tmdb:tv/1 Something 2001 | tmdb:tv/2 Other 2002") {
				t.Fatalf("low confidence row must list candidates: %q", l)
			}
		}
	}
	if inception == "" || substance == "" {
		t.Fatalf("missing rows:\n%s", tsv)
	}

	// edit: pick a candidate by hand for one row, ignore the other
	edited := strings.Replace(tsv, substance, root+"/某种物质\tmovie\ttmdb\tmovie/933260\t某种物质\t2024\t1.00\tchosen", 1)
	edited = strings.Replace(edited, inception, root+"/盗梦空间\tignore\t\t\t\t\t1.00\tnot mine", 1)
	planFile := filepath.Join(t.TempDir(), "plan.tsv")
	if err := os.WriteFile(planFile, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	// dry run: nothing is written
	mediaFlags.dryRun = true
	out, err := run(t, runMediaApply, planFile)
	if err != nil || len(f.sets) != 0 || f.uploads != 0 || !strings.Contains(out, "would") {
		t.Fatalf("dry run: err=%v sets=%d uploads=%d out=%q", err, len(f.sets), f.uploads, out)
	}

	// real apply without --env: the server does the scraping, then the row is pinned
	mediaFlags.dryRun = false
	if _, err := run(t, runMediaApply, planFile); err != nil {
		t.Fatal(err)
	}
	if len(f.scrapes) != 1 || f.scrapes[0]["id"] != "movie/933260" || f.scrapes[0]["force"] != true || f.scrapes[0]["lock"] != true {
		t.Fatalf("server scrape calls: %v", f.scrapes)
	}
	ign := f.items[root+"/盗梦空间"]
	if *ign.Kind != "ignore" || !*ign.Locked {
		t.Fatalf("ignore row: %+v", ign)
	}
	sub := f.items[root+"/某种物质"]
	if *sub.Source != "manual" || !*sub.Locked || *sub.Kind != "movie" {
		t.Fatalf("applied row: %+v", sub)
	}
	// idempotent
	before := len(f.items)
	if _, err := run(t, runMediaApply, planFile); err != nil || len(f.items) != before {
		t.Fatalf("second apply: %v (%d items, was %d)", err, len(f.items), before)
	}

	// ls
	out, err = run(t, runMediaLs)
	if err != nil || !strings.Contains(out, root+"/某种物质\tmovie") {
		t.Fatalf("ls: %v %q", err, out)
	}
}

func TestApplyWithEnvFetchesAndUploadsFromThisMachine(t *testing.T) {
	f, srv := newFakeOP(t)
	pointAtFakeOP(t, srv)

	img := image.NewRGBA(image.Rect(0, 0, 20, 30))
	for x := 0; x < 20; x++ {
		img.Set(x, 3, color.White)
	}
	var pngb bytes.Buffer
	_ = png.Encode(&pngb, img)
	var providerHits int
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHits++
		switch {
		case r.URL.Path == "/3/tv/890":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":890,"name":"新世纪福音战士","original_name":"Neon Genesis Evangelion","first_air_date":"1995-10-04","overview":"o","poster_path":"/p.png","backdrop_path":"/b.png","vote_average":8.3,"genres":[{"name":"动画"}],"external_ids":{"imdb_id":"tt0112159"},"seasons":[{"season_number":1}]}`))
		case r.URL.Path == "/3/tv/890/season/1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"episodes":[{"episode_number":1,"name":"使徒，来袭"},{"episode_number":2,"name":"见知的天花板"}]}`))
		case strings.HasPrefix(r.URL.Path, "/img/"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngb.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer prov.Close()
	mediaConfigHook = func(c *scraper.Config) {
		c.BaseURLs = map[string]string{"tmdb": prov.URL + "/3", "tmdb_image": prov.URL + "/img"}
		c.RatePerSec = map[string]float64{"tmdb": 1000}
		c.OMDbKeys, c.DoubanAPIKey = nil, ""
	}
	mediaAllowPrivate = true
	t.Cleanup(func() { mediaConfigHook, mediaAllowPrivate = nil, false })

	envFile := filepath.Join(t.TempDir(), "media.env")
	_ = os.WriteFile(envFile, []byte("TMDB_READ_TOKEN=eyJfake.token.value\n"), 0o600)
	mediaFlags.env = envFile

	f.dir("/lib/EVA", d("Season 1"))
	f.dir("/lib/EVA/Season 1", v("EVA.S01E01.mkv"), v("EVA.S01E02.mkv"), v("EVA.S01E03.mkv"))
	planFile := filepath.Join(t.TempDir(), "plan.tsv")
	_ = os.WriteFile(planFile, []byte("/lib/EVA\ttv\ttmdb\ttv/890\t\t\t0.9\t\n"), 0o600)

	if _, err := run(t, runMediaApply, planFile); err != nil {
		t.Fatal(err)
	}
	if len(f.scrapes) != 0 {
		t.Fatal("the server was asked to scrape although this machine had keys")
	}
	got := f.items["/lib/EVA"]
	if *got.Title != "新世纪福音战士" || *got.Year != 1995 || got.ExternalIDs["imdb"] != "tt0112159" || got.ExternalIDs["tmdb"] != "tv/890" {
		t.Fatalf("details: %+v", got)
	}
	if got.PosterID == nil || *got.PosterID == 0 || got.BackdropID == nil || !*got.Locked || *got.Source != "manual" {
		t.Fatalf("images / lock / source: %+v", got)
	}
	// both images are the same bytes here, so they deduplicate to one asset
	if f.uploads != 2 || len(f.assets) != 1 {
		t.Fatalf("uploads=%d distinct assets=%d", f.uploads, len(f.assets))
	}
	if len(got.Episodes) != 3 || got.Episodes[0].FileName != "Season 1/EVA.S01E01.mkv" || got.Episodes[0].Title != "使徒，来袭" || got.Episodes[2].Title != "" {
		t.Fatalf("episodes: %+v", got.Episodes)
	}
}

func TestTokenFile(t *testing.T) {
	_, srv := newFakeOP(t)
	old := syncFlags
	t.Cleanup(func() { syncFlags = old })
	p := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(p, []byte("file-token\r\n"), 0o600)
	syncFlags.server, syncFlags.token, syncFlags.tokenFile = srv.URL, "", p
	c, err := newSyncClient()
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Total int `json:"total"`
	}
	if err := c.Call("GET", "/api/admin/media/list", nil, &out); err != nil {
		t.Fatalf("the token from the file was not used: %v", err)
	}
	_ = os.WriteFile(p, []byte("  \n"), 0o600)
	if _, err := newSyncClient(); err == nil {
		t.Fatal("an empty token file was accepted")
	}
	syncFlags.tokenFile = filepath.Join(t.TempDir(), "missing")
	if _, err := newSyncClient(); err == nil || strings.Contains(err.Error(), "file-token") {
		t.Fatalf("missing token file: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// tinyMKV is an EBML header, an unknown-size Segment and a Tracks element with
// one ASS subtitle track.
func tinyMKV() []byte {
	b := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x84, 0x42, 0x82, 0x81, 'm'}
	b = append(b, 0x18, 0x53, 0x80, 0x67, 0xFF)
	entry := append([]byte{0xD7, 0x81, 0x01, 0x83, 0x81, 0x11, 0x86, 0x8A}, []byte("S_TEXT/ASS")...)
	entry = append(entry, 0x22, 0xB5, 0x9C, 0x83, 'c', 'h', 'i')
	tracks := append([]byte{0xAE, 0x80 | byte(len(entry))}, entry...)
	b = append(b, 0x16, 0x54, 0xAE, 0x6B, 0x80|byte(len(tracks)))
	b = append(b, tracks...)
	return append(b, 0x1F, 0x43, 0xB6, 0x75, 0x80)
}

func TestCheckReportsMissingEpisodesAndProbes(t *testing.T) {
	f, srv := newFakeOP(t)
	pointAtFakeOP(t, srv)
	var storageAuth, storageRange string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth, storageRange = r.Header.Get("Authorization"), r.Header.Get("Range")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(tinyMKV())
	}))
	defer storage.Close()
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/tv/1":
			_, _ = w.Write([]byte(`{"id":1,"name":"Show","first_air_date":"2020-01-01","seasons":[{"season_number":1}]}`))
		case "/3/tv/1/season/1":
			_, _ = w.Write([]byte(`{"episodes":[{"episode_number":1,"air_date":"2020-01-01"},{"episode_number":2,"air_date":"2020-01-08"},{"episode_number":3,"air_date":"2020-01-15"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer prov.Close()
	mediaConfigHook = func(c *scraper.Config) {
		c.BaseURLs = map[string]string{"tmdb": prov.URL + "/3"}
		c.RatePerSec = map[string]float64{"tmdb": 1000}
		c.OMDbKeys, c.DoubanAPIKey = nil, ""
	}
	t.Cleanup(func() { mediaConfigHook = nil })
	envFile := filepath.Join(t.TempDir(), "media.env")
	_ = os.WriteFile(envFile, []byte("TMDB_READ_TOKEN=eyJfake.token.value\n"), 0o600)
	out := filepath.Join(t.TempDir(), "report.md")
	mediaFlags.env, mediaFlags.root, mediaFlags.probe, mediaFlags.out = envFile, "/lib", true, out

	f.rawURL = storage.URL + "/raw"
	f.items["/lib/Show"] = mm.MediaSetReq{Path: "/lib/Show", Kind: ptr("tv")}
	f.items["/lib/Skip"] = mm.MediaSetReq{Path: "/lib/Skip", Kind: ptr("ignore")}
	f.ids = map[string]map[string]string{"/lib/Show": {"tmdb": "tv/1"}}
	f.dir("/lib/Show", d("Season 1"))
	f.dir("/lib/Show/Season 1", v("Show.S01E01.mkv"), v("Show.S01E03.mkv"))

	if _, err := run(t, runMediaCheck); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	rep := string(b)
	for _, want := range []string{"⚠️缺集", "缺 S01：2", "预期 3 集（来源：tmdb）", "S_TEXT/ASS/chi", "/lib/Show"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	if strings.Contains(rep, "/lib/Skip") {
		t.Error("an ignored item was checked")
	}
	if len(f.gets) != 1 || f.gets[0] != "/lib/Show/Season 1/Show.S01E01.mkv" {
		t.Errorf("one get on the first episode expected: %v", f.gets)
	}
	if storageRange == "" || storageAuth != "" {
		t.Errorf("storage saw Range=%q Authorization=%q", storageRange, storageAuth)
	}
	for _, l := range f.lists {
		if l != "/lib/Show" && l != "/lib/Show/Season 1" {
			t.Errorf("unexpected listing %s", l)
		}
	}
	if len(f.sets) != 0 || f.uploads != 0 {
		t.Error("check must not write")
	}
}

func TestLibraryAddLsRm(t *testing.T) {
	f, srv := newFakeOP(t)
	pointAtFakeOP(t, srv)
	mediaFlags.path = "/115/arc/MOV/Anime/"
	if out, err := run(t, runLibraryAdd); err != nil || !strings.Contains(out, "/115/arc/MOV/Anime is a library") {
		t.Fatalf("add: %q %v", out, err)
	}
	got := f.items["/115/arc/MOV/Anime"]
	if got.Kind == nil || *got.Kind != "library" || got.Locked == nil || !*got.Locked || got.Source == nil || *got.Source != "manual" || *got.Title != "Anime" {
		t.Fatalf("stored: %+v", got)
	}
	f.items["/115/arc/MOV/Anime/EVA"] = mm.MediaSetReq{Path: "/115/arc/MOV/Anime/EVA", Kind: ptr("tv")}
	if out, _ := run(t, runLibraryLs); strings.TrimSpace(out) != "/115/arc/MOV/Anime" {
		t.Fatalf("ls: %q", out)
	}
	mediaFlags.path = "/115/arc/MOV/Anime/EVA"
	if _, err := run(t, runLibraryRm); err == nil {
		t.Fatal("rm removed a work")
	}
	if _, err := run(t, runLibraryAdd); err == nil {
		t.Fatal("add overwrote a work")
	}
	mediaFlags.path = "/115/arc/MOV/Anime"
	if _, err := run(t, runLibraryRm); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.items["/115/arc/MOV/Anime"]; ok || f.items["/115/arc/MOV/Anime/EVA"].Path == "" {
		t.Fatalf("rm should delete only the root: %v", f.items)
	}
}
