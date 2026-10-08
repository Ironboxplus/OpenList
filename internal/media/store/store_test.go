package store

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	conf.Conf = conf.DefaultConfig(t.TempDir())
	name := fmt.Sprintf("file:media-store-%d?mode=memory&cache=shared", time.Now().UnixNano())
	d, err := gorm.Open(sqlite.Open(name), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Init(d)
	return New(t.TempDir())
}

func str(s string) *string   { return &s }
func i32(f float32) *float32 { return &f }
func boolp(b bool) *bool     { return &b }
func intp(i int) *int        { return &i }

func pngBytes(t *testing.T, w, h int, c color.Color) []byte {
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

func TestSetOnlyOverwritesGivenFields(t *testing.T) {
	s := newTestStore(t)
	it, err := s.Set(model.MediaSetReq{
		Path: "/115/云下载/EVA/", Kind: str("tv"), Title: str("EVA"), Year: intp(1995),
		Overview: str("old overview"), ExternalIDs: map[string]string{"tmdb": "tv/890"},
	}, SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if it.Path != "/115/云下载/EVA" {
		t.Fatalf("path not normalised: %q", it.Path)
	}
	// second write names only the title and the lock
	if _, err := s.Set(model.MediaSetReq{Path: "/115/云下载/EVA", Title: str("新世纪福音战士"), Locked: boolp(true)}, SetOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("/115/云下载/EVA")
	if got.Title != "新世纪福音战士" || !got.Locked {
		t.Fatalf("given fields not written: %+v", got)
	}
	if got.Kind != "tv" || got.Year != 1995 || got.Overview != "old overview" || got.ExternalIDs != `{"tmdb":"tv/890"}` {
		t.Fatalf("fields not in the request were changed: %+v", got)
	}
	// an explicitly empty overview does clear it
	if _, err := s.Set(model.MediaSetReq{Path: "/115/云下载/EVA", Overview: str("")}, SetOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get("/115/云下载/EVA")
	if got.Overview != "" || got.Title != "新世纪福音战士" {
		t.Fatalf("explicit empty not applied or collateral change: %+v", got)
	}
}

func TestSetValidation(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Set(model.MediaSetReq{Path: "/a"}, SetOptions{}); err == nil {
		t.Fatal("new item without kind accepted")
	}
	if _, err := s.Set(model.MediaSetReq{Path: "/a", Kind: str("bogus")}, SetOptions{}); err == nil {
		t.Fatal("invalid kind accepted")
	}
	if _, err := s.Set(model.MediaSetReq{Path: "/", Kind: str("movie")}, SetOptions{}); err == nil {
		t.Fatal("root accepted")
	}
}

func TestLockedItemRefusesAutomaticWrite(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Set(model.MediaSetReq{Path: "/m/a", Kind: str("movie"), Title: str("mine"), Locked: boolp(true)}, SetOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Set(model.MediaSetReq{Path: "/m/a", Title: str("scraped"), Source: str("scraper")}, SetOptions{RespectLock: true})
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	got, _ := s.Get("/m/a")
	if got.Title != "mine" {
		t.Fatalf("locked title overwritten: %q", got.Title)
	}
	// the admin's explicit edit still goes through
	if _, err := s.Set(model.MediaSetReq{Path: "/m/a", Title: str("edited")}, SetOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestEpisodesReplacedOnlyWhenGiven(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Set(model.MediaSetReq{Path: "/tv/x", Kind: str("tv"), Episodes: []model.MediaEpisodeIn{
		{FileName: "S01E01.mkv", Season: 1, Episode: 1, Title: "a"},
		{FileName: "S01E02.mkv", Season: 1, Episode: 2, Title: "b"},
	}}, SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	it, _ := s.Get("/tv/x")
	if eps, _ := db.GetMediaEpisodes(it.ID); len(eps) != 2 {
		t.Fatalf("episodes = %d", len(eps))
	}
	if _, err := s.Set(model.MediaSetReq{Path: "/tv/x", Title: str("t")}, SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if eps, _ := db.GetMediaEpisodes(it.ID); len(eps) != 2 {
		t.Fatalf("episodes lost by a set without episodes: %d", len(eps))
	}
	if _, err := s.Set(model.MediaSetReq{Path: "/tv/x", Episodes: []model.MediaEpisodeIn{{FileName: "S01E03.mkv", Season: 1, Episode: 3}}}, SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if eps, _ := db.GetMediaEpisodes(it.ID); len(eps) != 1 || eps[0].Episode != 3 {
		t.Fatalf("episodes not replaced: %+v", eps)
	}
}

func TestChildrenAreDirectOnlyAndLikeIsEscaped(t *testing.T) {
	s := newTestStore(t)
	for _, p := range []string{"/lib/A", "/lib/B", "/lib/A/S1", "/lib/100%_x", "/libx/C", "/lib/B/deep/er"} {
		if _, err := s.Set(model.MediaSetReq{Path: p, Kind: str("movie")}, SetOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	kids, err := db.ListMediaChildren("/lib")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range kids {
		got = append(got, k.Path)
	}
	want := []string{"/lib/100%_x", "/lib/A", "/lib/B"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("children = %v, want %v", got, want)
	}
	// a LIKE wildcard in the parent path must not match siblings
	kids, _ = db.ListMediaChildren("/l_b")
	if len(kids) != 0 {
		t.Fatalf("'_' acted as a wildcard: %v", kids)
	}
	n, err := s.DeletePrefix("/lib/A")
	if err != nil || n != 2 {
		t.Fatalf("DeletePrefix = %d, %v; want 2", n, err)
	}
	if it, _ := s.Get("/libx/C"); it == nil {
		t.Fatal("prefix delete leaked into /libx")
	}
}

func TestPendingFilter(t *testing.T) {
	s := newTestStore(t)
	mk := func(p string, conf float32, locked bool, cands []model.MediaCandidate) {
		if _, err := s.Set(model.MediaSetReq{Path: p, Kind: str("movie"), Confidence: i32(conf), Locked: boolp(locked), Candidates: cands}, SetOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mk("/p/low", 0.3, false, nil)
	mk("/p/high", 0.9, false, nil)
	mk("/p/highcand", 0.9, false, []model.MediaCandidate{{Provider: "tmdb", ID: "tv/1"}})
	mk("/p/lowlocked", 0.1, true, nil)
	mk("/p/emptycand", 0.9, false, []model.MediaCandidate{})
	items, total, err := db.ListMediaItems(db.MediaListFilter{Prefix: "/p", Pending: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Path)
	}
	if total != 2 || fmt.Sprint(got) != "[/p/highcand /p/low]" {
		t.Fatalf("pending = %v (total %d)", got, total)
	}
}

func TestAssetDedupBySHA(t *testing.T) {
	s := newTestStore(t)
	data := pngBytes(t, 600, 900, color.RGBA{200, 10, 10, 255})
	a, err := s.AddAsset(data, "https://img.example/a.png")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AddAsset(data, "https://img.example/other-url.png")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.SHA256 != b.SHA256 {
		t.Fatalf("same bytes got two assets: %d vs %d", a.ID, b.ID)
	}
	if a.Width != 600 || a.Height != 900 || a.Mime != "image/png" {
		t.Fatalf("asset meta: %+v", a)
	}
	for _, thumb := range []bool{false, true} {
		if _, err := os.Stat(s.AssetFile(a, thumb)); err != nil {
			t.Fatalf("file missing (thumb=%v): %v", thumb, err)
		}
	}
	f, _ := os.Open(s.AssetFile(a, true))
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width != ThumbWidth {
		t.Fatalf("thumb = %+v, %v; want width %d", cfg, err, ThumbWidth)
	}
	other, err := s.AddAsset(pngBytes(t, 50, 50, color.RGBA{0, 0, 200, 255}), "")
	if err != nil || other.ID == a.ID {
		t.Fatalf("different bytes shared an asset: %v", err)
	}
	if _, err := s.AddAsset([]byte("not an image"), ""); err == nil {
		t.Fatal("garbage accepted as an image")
	}
}

func TestLibraryKindIsAccepted(t *testing.T) {
	s := newTestStore(t)
	it, err := s.Set(model.MediaSetReq{Path: "/lib", Kind: str("library"), Title: str("Lib")}, SetOptions{})
	if err != nil || it.Kind != "library" {
		t.Fatalf("library rejected: %v", err)
	}
	if items, total, err := db.ListMediaItems(db.MediaListFilter{Kind: "library", Page: 1, PerPage: 10}); err != nil || total != 1 || items[0].Path != "/lib" {
		t.Fatalf("list by kind: %v %d", err, total)
	}
}
