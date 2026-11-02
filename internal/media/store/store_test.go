package store

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	conf.Conf = conf.DefaultConfig(t.TempDir())
	old := conf.DataDir
	conf.DataDir = t.TempDir()
	t.Cleanup(func() { conf.DataDir = old })
	d, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:store-%d?mode=memory&cache=shared", time.Now().UnixNano())),
		&gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "x_"}})
	if err != nil {
		t.Fatal(err)
	}
	db.Init(d)
	return New("runtime", "/imgs")
}

func ptr[T any](v T) *T { return &v }

func png1(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		img.Set(x, x, color.RGBA{255, 0, 0, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSetCreatesAWorkOnTheUpstreamModel(t *testing.T) {
	s := newStore(t)
	got, err := s.Set(model.MediaSetReq{
		Path: "/lib/EVA", Kind: ptr("anime"), Title: ptr("新世纪福音战士"), OriginalTitle: ptr("Neon Genesis Evangelion"), Year: ptr(1995),
		ExternalIDs: map[string]string{"tmdb": "tv/890", "imdb": "tt0112159"},
		Ratings:     map[string]any{"tmdb": 8.3, "imdb": 8.5, "rt": "93%"},
		Genres:      []string{"动画", "科幻"}, Overview: ptr("o"), Source: ptr("scraper"), Confidence: ptr(float32(0.9)),
	}, SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.MediaType != model.MediaTypeVideo || got.FolderPath != "/lib" || got.FileName != "EVA" || !got.IsFolder || got.AlbumName != "" {
		t.Fatalf("identity: %+v", got)
	}
	if got.VideoType != "anime" || got.ScrapedName != "新世纪福音战士" || got.OriginalTitle != "Neon Genesis Evangelion" ||
		got.ReleaseDate != "1995-01-01" || got.Genre != "动画,科幻" || got.Plot != "o" || got.ExternalID != "tmdb:890" {
		t.Fatalf("fields: %+v", got)
	}
	if got.Rating != 8.3 || got.ScrapedAt == nil || got.Source != "scraper" || got.Confidence != 0.9 {
		t.Fatalf("rating/source: rating=%v scrapedAt=%v source=%q", got.Rating, got.ScrapedAt, got.Source)
	}
	rs, _ := db.ListMediaRatings(got.ID)
	if m := RatingsToMap(rs[got.ID]); len(m) != 3 || m["rt"] != "93%" || m["imdb"] != float32(8.5) {
		t.Fatalf("ratings: %v", m)
	}
	// a file at the top is a file, not a folder
	f, err := s.Set(model.MediaSetReq{Path: "/lib/Film (2010).mkv", Kind: ptr("movie")}, SetOptions{})
	if err != nil || f.IsFolder || f.ScrapedName != "Film (2010)" {
		t.Fatalf("file work: %+v %v", f, err)
	}
}

func TestSetOnlyWritesTheGivenFields(t *testing.T) {
	s := newStore(t)
	if _, err := s.Set(model.MediaSetReq{Path: "/lib/A", Kind: ptr("tv"), Title: ptr("A"), Year: ptr(2001), Overview: ptr("keep"),
		Ratings: map[string]any{"tmdb": 7.0}}, SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(model.MediaSetReq{Path: "/lib/A", Title: ptr("B")}, SetOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("/lib/A")
	if got.ScrapedName != "B" || got.VideoType != "tv" || got.ReleaseDate != "2001-01-01" || got.Plot != "keep" || got.Rating != 7 {
		t.Fatalf("%+v", got)
	}
	rs, _ := db.ListMediaRatings(got.ID)
	if len(rs[got.ID]) != 1 {
		t.Fatalf("ratings not in the request must stay: %+v", rs)
	}
	// ratings given as an empty set clear them
	if _, err := s.Set(model.MediaSetReq{Path: "/lib/A", Ratings: map[string]any{}}, SetOptions{}); err != nil {
		t.Fatal(err)
	}
	rs, _ = db.ListMediaRatings(got.ID)
	if len(rs[got.ID]) != 0 {
		t.Fatalf("an empty ratings set must clear: %+v", rs)
	}
	if _, err := s.Set(model.MediaSetReq{Path: "/lib/Nope"}, SetOptions{}); err == nil {
		t.Error("a new work without a kind must be refused")
	}
	if _, err := s.Set(model.MediaSetReq{Path: "/lib/A", Kind: ptr("library")}, SetOptions{}); err == nil {
		t.Error("library is not a kind of work")
	}
	if _, err := s.Set(model.MediaSetReq{Path: "/", Kind: ptr("movie")}, SetOptions{}); err == nil {
		t.Error("the root cannot be a work")
	}
}

func TestSetRespectsTheLockOnlyWhenAsked(t *testing.T) {
	s := newStore(t)
	if _, err := s.Set(model.MediaSetReq{Path: "/lib/L", Kind: ptr("movie"), Title: ptr("mine"), Locked: ptr(true)}, SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(model.MediaSetReq{Path: "/lib/L", Title: ptr("auto")}, SetOptions{RespectLock: true}); err != ErrLocked {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if got, _ := s.Get("/lib/L"); got.ScrapedName != "mine" {
		t.Fatalf("locked title changed: %q", got.ScrapedName)
	}
	if _, err := s.Set(model.MediaSetReq{Path: "/lib/L", Title: ptr("admin")}, SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("/lib/L"); got.ScrapedName != "admin" {
		t.Fatalf("an admin edit must go through: %q", got.ScrapedName)
	}
}

func TestIgnoreHidesAndLeavingIgnoreShows(t *testing.T) {
	s := newStore(t)
	got, err := s.Set(model.MediaSetReq{Path: "/lib/OST", Kind: ptr("ignore")}, SetOptions{})
	if err != nil || !got.Hidden {
		t.Fatalf("ignored work must be hidden: %+v %v", got, err)
	}
	got, err = s.Set(model.MediaSetReq{Path: "/lib/OST", Kind: ptr("movie")}, SetOptions{})
	if err != nil || got.Hidden {
		t.Fatalf("a work no longer ignored must be shown: %+v %v", got, err)
	}
}

func TestSetEpisodesFillsBothTables(t *testing.T) {
	s := newStore(t)
	got, err := s.Set(model.MediaSetReq{Path: "/lib/Show", Kind: ptr("tv"), Episodes: []model.MediaEpisodeIn{
		{FileName: "Season 1/Show.S01E02.mkv", Season: 1, Episode: 2, Title: "two"},
		{FileName: "Season 1/Show.S01E01.mkv", Season: 1, Episode: 1, Title: "one", Overview: "o", Still: "thumb:/imgs/s.jpg"},
	}}, SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eps, _ := db.ListMediaEpisodes(got.ID)
	if len(eps) != 2 || eps[0].Title != "one" || eps[0].Still != "thumb:/imgs/s.jpg" {
		t.Fatalf("rows: %+v", eps)
	}
	if !strings.Contains(got.Episodes, `"file_name":"Season 1/Show.S01E01.mkv"`) || !strings.Contains(got.Episodes, `"index":2`) {
		t.Fatalf("upstream JSON: %s", got.Episodes)
	}
}

func TestSaveImageKeepsPosterAndBackdropApart(t *testing.T) {
	s := newStore(t)
	data := png1(t)
	poster, err := s.SaveImage(context.Background(), "/lib/EVA", "poster", data)
	if err != nil {
		t.Fatal(err)
	}
	backdrop, err := s.SaveImage(context.Background(), "/lib/EVA", "backdrop", data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(poster, "thumb:/imgs/") || !strings.HasPrefix(backdrop, "thumb:/imgs/") || poster == backdrop {
		t.Fatalf("poster=%q backdrop=%q", poster, backdrop)
	}
	for _, ref := range []string{poster, backdrop} {
		if _, err := os.Stat(filepath.Join(conf.DataDir, filepath.FromSlash(strings.TrimPrefix(ref, "thumb:/")))); err != nil {
			t.Errorf("%s not on disk: %v", ref, err)
		}
	}
	if _, err := s.SaveImage(context.Background(), "/lib/EVA", "poster", []byte("not an image at all")); err == nil {
		t.Error("a non-image was stored")
	}
	if _, err := s.SaveImage(context.Background(), "/lib/EVA", "poster", make([]byte, MaxImageBytes+1)); err == nil {
		t.Error("an oversized image was stored")
	}
}

func TestDeleteAndDeletePrefix(t *testing.T) {
	s := newStore(t)
	for _, p := range []string{"/d/a", "/d/a/s1", "/d/b", "/dd/x", "/e/z"} {
		if _, err := s.Set(model.MediaSetReq{Path: p, Kind: ptr("tv")}, SetOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.DeletePrefix("/d")
	if err != nil || n != 3 {
		t.Fatalf("deleted %d, %v; /d, /d/a/s1 and /d/b go, /dd/x stays", n, err)
	}
	if it, _ := s.Get("/dd/x"); it == nil {
		t.Fatal("a sibling with the same leading characters was deleted")
	}
	if n, _ := s.Delete("/e/z"); n != 1 {
		t.Fatalf("single delete = %d", n)
	}
	if n, _ := s.Delete("/e/z"); n != 0 {
		t.Fatalf("deleting twice = %d", n)
	}
}
