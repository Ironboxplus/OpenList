package strm

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// pngURI is a data URI of an image that LoadImage accepts; n varies its size.
func pngURI(n int) (string, []byte) {
	b := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, n)...)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b), b
}

type mediaRig struct {
	t *testing.T
	d *Strm
}

func newMediaRig(t *testing.T, paths string, edit func(*Strm)) *mediaRig {
	t.Helper()
	conf.Conf = conf.DefaultConfig(t.TempDir())
	g, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:strm-media-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Init(g)
	d := &Strm{Addition: Addition{Paths: paths, Source: SourceMedia, SiteUrl: "http://site", EncodePath: true}}
	if edit != nil {
		edit(d)
	}
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.recheck = 0 // look at the library on every call
	return &mediaRig{t: t, d: d}
}

func (r *mediaRig) save(it *model.MediaItem, eps []model.MediaEpisode, subs []model.MediaSidecar) *model.MediaItem {
	r.t.Helper()
	it.MediaType, it.ScanPathID = firstNonEmpty(it.MediaType, model.MediaTypeVideo), 1
	if err := db.SaveVideoItem(it, nil, eps); err != nil {
		r.t.Fatal(err)
	}
	if subs != nil {
		if err := db.ReplaceMediaSidecars(it.ID, subs); err != nil {
			r.t.Fatal(err)
		}
	}
	return it
}

func firstNonEmpty(a, b model.MediaType) model.MediaType {
	if a != "" {
		return a
	}
	return b
}

func (r *mediaRig) objs(path string) []model.Obj {
	r.t.Helper()
	objs, err := r.d.List(context.Background(), &model.Object{Path: path, IsFolder: true}, model.ListArgs{})
	if err != nil {
		r.t.Fatalf("List %s: %v", path, err)
	}
	return objs
}

func (r *mediaRig) names(path string) []string {
	r.t.Helper()
	var out []string
	for _, o := range r.objs(path) {
		out = append(out, o.GetName())
	}
	return out
}

// read is the content of the virtual file at path, and its listed size.
func (r *mediaRig) read(path string) (string, int64) {
	r.t.Helper()
	ctx := context.Background()
	obj, err := r.d.Get(ctx, path)
	if err != nil {
		r.t.Fatalf("Get %s: %v", path, err)
	}
	link, err := r.d.Link(ctx, obj, model.LinkArgs{})
	if err != nil {
		r.t.Fatalf("Link %s: %v", path, err)
	}
	rc, err := link.RangeReader.RangeRead(ctx, http_range.Range{Length: -1})
	if err != nil {
		r.t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b), obj.GetSize()
}

func scrapedMovie(folder, file, name, date, tmdb string) *model.MediaItem {
	now := time.Now()
	it := &model.MediaItem{FolderPath: folder, FileName: file, ScrapedName: name, ReleaseDate: date, VideoType: model.VideoTypeMovie, ScrapedAt: &now}
	it.ExternalIDs = `{"tmdb":"movie/` + tmdb + `","imdb":"tt` + tmdb + `"}`
	return it
}

func TestMediaModeTreeFilesAndContent(t *testing.T) {
	r := newMediaRig(t, "lib:/media", func(d *Strm) { d.WithSign = false })
	cover, coverBytes := pngURI(10)
	back, backBytes := pngURI(20)
	still, stillBytes := pngURI(5)

	// a film file with two subtitles, a poster and a backdrop
	inc := scrapedMovie("/media/films", "Inception.mkv", "Inception", "2010-07-16", "27205")
	inc.Plot, inc.Genre, inc.Rating, inc.Authors = "A thief & his team.", "Action,Sci-Fi", 8.4, `["Leonardo DiCaprio"]`
	inc.Cover, inc.Backdrop = cover, back
	r.save(inc, nil, []model.MediaSidecar{
		{Video: "Inception.mkv", Path: "Inception.chs.ass", Size: 7},
		{Video: "Inception.mkv", Path: "Inception.srt", Size: 9},
	})
	// a film folder with versions: the first version is the film
	ven := scrapedMovie("/media/films", "Venom", "Venom", "2018-10-03", "335983")
	ven.IsFolder = true
	ven.Episodes = model.VersionsJSON([]string{"Venom.120fps.mkv", "Venom.60fps.mkv"})
	r.save(ven, nil, []model.MediaSidecar{{Video: "Venom.120fps.mkv", Path: "Venom.120fps.eng.srt", Size: 3}})
	// a show in one folder holding two seasons, one subtitle, one still
	show := scrapedMovie("/media/tv", "Show.Complete", "Show", "2011-04-17", "1399")
	show.VideoType, show.IsFolder, show.Season = model.VideoTypeTV, true, 1
	show.ExternalIDs = `{"tmdb":"tv/1399"}`
	show.Cover = cover
	r.save(show, []model.MediaEpisode{
		{FileName: "Season 1/a.S01E01.mkv", Season: 1, Episode: 1, Title: "Pilot", Overview: "It starts.", Still: still},
		{FileName: "Season 1/a.S01E02.mkv", Season: 1, Episode: 2},
		{FileName: "Season 2/a.S02E01.mkv", Season: 2, Episode: 1, Title: "Again"},
	}, []model.MediaSidecar{{Video: "Season 1/a.S01E01.mkv", Path: "Season 1/a.S01E01.chs.ass", Size: 11}})
	// a track, which has no NFO
	track := &model.MediaItem{MediaType: model.MediaTypeMusic, FolderPath: "/media/music", FileName: "t.flac", ScrapedName: "Song", AlbumArtist: "Band", AlbumName: "Album", TrackNumber: 3}
	r.save(track, nil, nil)
	// not part of the tree: outside the path, hidden
	r.save(scrapedMovie("/other", "X.mkv", "X", "2000-01-01", "1"), nil, nil)
	hid := scrapedMovie("/media/films", "Hid.mkv", "Hidden", "2000-01-01", "2")
	hid.Hidden = true
	r.save(hid, nil, nil)

	if got, want := r.names("/"), []string{"Band", "Inception (2010)", "Show", "Venom (2018)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("root: %v", got)
	}
	want := []string{
		"Inception (2010).chs.ass", "Inception (2010).nfo", "Inception (2010).srt", "Inception (2010).strm",
		"fanart.jpg", "poster.jpg",
	}
	if got := r.names("/Inception (2010)"); !reflect.DeepEqual(got, want) {
		t.Fatalf("film dir: %v", got)
	}
	if got, want := r.names("/Venom (2018)"), []string{"Venom (2018).eng.srt", "Venom (2018).nfo", "Venom (2018).strm"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("version dir: %v", got)
	}
	if got, want := r.names("/Show"), []string{"Season 01", "Season 02", "poster.jpg", "tvshow.nfo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("show dir: %v", got)
	}
	if got, want := r.names("/Show/Season 01"), []string{
		"Show S01E01 - Pilot-thumb.jpg", "Show S01E01 - Pilot.chs.ass", "Show S01E01 - Pilot.nfo", "Show S01E01 - Pilot.strm",
		"Show S01E02.nfo", "Show S01E02.strm", "season.nfo",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("season dir: %v", got)
	}
	if got, want := r.names("/Show/Season 02"), []string{"Show S02E01 - Again.nfo", "Show S02E01 - Again.strm", "season.nfo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("season 2 dir: %v", got)
	}
	if got, want := r.names("/Band/Album"), []string{"03 - Song.strm"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("album dir: %v", got)
	}

	// .strm: the VFS path of the video, the default version for a folder of versions
	for p, want := range map[string]string{
		"/Inception (2010)/Inception (2010).strm":  "http://site/d/media/films/Inception.mkv",
		"/Venom (2018)/Venom (2018).strm":          "http://site/d/media/films/Venom/Venom.120fps.mkv",
		"/Show/Season 02/Show S02E01 - Again.strm": "http://site/d/media/tv/Show.Complete/Season%202/a.S02E01.mkv",
		"/Band/Album/03 - Song.strm":               "http://site/d/media/music/t.flac",
	} {
		got, size := r.read(p)
		if got != want || size != int64(len(want)) {
			t.Errorf("%s: %q (size %d), want %q", p, got, size, want)
		}
	}

	// every listed size is the size of what is served; images are read for it
	for _, c := range []struct{ path, data string }{
		{"/Inception (2010)/poster.jpg", string(coverBytes)},
		{"/Inception (2010)/fanart.jpg", string(backBytes)},
		{"/Show/Season 01/Show S01E01 - Pilot-thumb.jpg", string(stillBytes)},
	} {
		got, size := r.read(c.path)
		if got != c.data || size != int64(len(c.data)) {
			t.Errorf("%s: %d bytes, listed %d, want %d", c.path, len(got), size, len(c.data))
		}
	}
	for _, o := range r.objs("/Inception (2010)") {
		if o.GetName() == "poster.jpg" && o.GetSize() != int64(len(coverBytes)) {
			t.Errorf("listed size of the poster %d", o.GetSize())
		}
		if o.GetName() == "Inception (2010).chs.ass" && o.GetSize() != 7 {
			t.Errorf("listed size of the subtitle %d", o.GetSize())
		}
	}

	// NFOs: movie, show, season, episode
	nfoText, size := r.read("/Inception (2010)/Inception (2010).nfo")
	if size != int64(len(nfoText)) {
		t.Errorf("nfo size %d for %d bytes", size, len(nfoText))
	}
	for _, w := range []string{
		"<movie>", "<title>Inception</title>", "<year>2010</year>", "<plot>A thief &amp; his team.</plot>",
		"<genre>Action</genre>", "<genre>Sci-Fi</genre>", "<rating>8.4</rating>", "<premiered>2010-07-16</premiered>",
		"<name>Leonardo DiCaprio</name>", `<uniqueid type="tmdb" default="true">27205</uniqueid>`, `<uniqueid type="imdb">tt27205</uniqueid>`,
	} {
		if !strings.Contains(nfoText, w) {
			t.Errorf("movie nfo lacks %q:\n%s", w, nfoText)
		}
	}
	if s, _ := r.read("/Show/tvshow.nfo"); !strings.Contains(s, "<tvshow>") || !strings.Contains(s, `<uniqueid type="tmdb" default="true">1399</uniqueid>`) {
		t.Errorf("tvshow.nfo:\n%s", s)
	}
	if s, _ := r.read("/Show/Season 02/season.nfo"); !strings.Contains(s, "<seasonnumber>2</seasonnumber>") {
		t.Errorf("season.nfo:\n%s", s)
	}
	ep, _ := r.read("/Show/Season 01/Show S01E01 - Pilot.nfo")
	for _, w := range []string{"<episodedetails>", "<title>Pilot</title>", "<showtitle>Show</showtitle>", "<season>1</season>", "<episode>1</episode>", "<plot>It starts.</plot>"} {
		if !strings.Contains(ep, w) {
			t.Errorf("episode nfo lacks %q:\n%s", w, ep)
		}
	}

	// the subtitle takes the name of the virtual video, keeping the rest of its suffix
	objs := map[string]model.Obj{}
	for _, o := range r.objs("/Show/Season 01") {
		objs[o.GetName()] = o
	}
	if o := objs["Show S01E01 - Pilot.chs.ass"]; o == nil || o.GetSize() != 11 {
		t.Errorf("subtitle: %v", o)
	}

	// what is not there
	if _, err := r.d.Get(context.Background(), "/Inception (2010)/nothing.nfo"); err != errs.ObjectNotFound {
		t.Errorf("Get missing: %v", err)
	}
	if _, err := r.d.List(context.Background(), &model.Object{Path: "/Nope", IsFolder: true}, model.ListArgs{}); err != errs.ObjectNotFound {
		t.Errorf("List missing: %v", err)
	}
}

func TestMediaModeStrmContentWithSign(t *testing.T) {
	r := newMediaRig(t, "lib:/media", func(d *Strm) { d.WithSign = true; d.PathPrefix = "/d" })
	r.save(scrapedMovie("/media/films", "A B.mkv", "A B", "2001-01-01", "1"), nil, nil)
	got, size := r.read("/A B (2001)/A B (2001).strm")
	want := "http://site/d/media/films/A%20B.mkv?sign=" + sign.Sign("/media/films/A B.mkv")
	if got != want || size != int64(len(want)) {
		t.Errorf("got %q (size %d), want %q", got, size, want)
	}
}

func TestMediaModeUnreadableImageIsLeftOut(t *testing.T) {
	r := newMediaRig(t, "lib:/media", nil)
	it := scrapedMovie("/media/films", "A.mkv", "A", "2001-01-01", "1")
	it.Cover = "http://127.0.0.1/poster.jpg" // not an allowed host
	it.Backdrop = "data:image/png;base64,AAAA"
	r.save(it, nil, nil)
	if got, want := r.names("/A (2001)"), []string{"A (2001).nfo", "A (2001).strm"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	if _, err := r.d.Get(context.Background(), "/A (2001)/poster.jpg"); err != errs.ObjectNotFound {
		t.Errorf("Get: %v", err)
	}
}

func TestMediaModeRebuildsWhenTheLibraryChanges(t *testing.T) {
	r := newMediaRig(t, "lib:/media", nil)
	a := r.save(scrapedMovie("/media/films", "A.mkv", "Alpha", "2001-01-01", "1"), nil, nil)
	if got := r.names("/"); !reflect.DeepEqual(got, []string{"Alpha (2001)"}) {
		t.Fatalf("first: %v", got)
	}
	// the same tree is served while nothing changed
	tree := r.d.tree
	r.names("/")
	if r.d.tree != tree {
		t.Error("rebuilt without a change")
	}

	// an update of a work (its name) changes the latest updated_at
	time.Sleep(10 * time.Millisecond)
	a.ScrapedName = "Beta"
	r.save(a, nil, nil)
	if got := r.names("/"); !reflect.DeepEqual(got, []string{"Beta (2001)"}) {
		t.Fatalf("after update: %v", got)
	}
	// a new work changes the count
	r.save(scrapedMovie("/media/films", "C.mkv", "Gamma", "2003-01-01", "3"), nil, nil)
	if got := r.names("/"); !reflect.DeepEqual(got, []string{"Beta (2001)", "Gamma (2003)"}) {
		t.Fatalf("after add: %v", got)
	}
	// a subtitle recorded later changes the sidecar count
	if err := db.ReplaceMediaSidecars(a.ID, []model.MediaSidecar{{Video: "A.mkv", Path: "A.srt", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	if got := r.names("/Beta (2001)"); !reflect.DeepEqual(got, []string{"Beta (2001).nfo", "Beta (2001).srt", "Beta (2001).strm"}) {
		t.Fatalf("after sidecar: %v", got)
	}
	// a work removed
	if err := db.DeleteMediaItem(a.ID); err != nil {
		t.Fatal(err)
	}
	if got := r.names("/"); !reflect.DeepEqual(got, []string{"Gamma (2003)"}) {
		t.Fatalf("after delete: %v", got)
	}
}

func TestMediaModeSeveralRootsAndSamePath(t *testing.T) {
	r := newMediaRig(t, "films:/media/films\ntv:/media/tv", nil)
	r.save(scrapedMovie("/media/films", "A.mkv", "A", "2001-01-01", "1"), nil, nil)
	// two works of the same title and year in one folder: the first keeps the name
	r.save(scrapedMovie("/media/films/dup", "A.mkv", "A", "2001-01-01", "1"), nil, nil)
	r.save(scrapedMovie("/media/tv", "B.mkv", "B", "2002-01-01", "2"), nil, nil)
	if got := r.names("/"); !reflect.DeepEqual(got, []string{"films", "tv"}) {
		t.Fatalf("roots: %v", got)
	}
	if got := r.names("/films"); !reflect.DeepEqual(got, []string{"A (2001)"}) {
		t.Fatalf("films: %v", got)
	}
	if got := r.names("/tv"); !reflect.DeepEqual(got, []string{"B (2002)"}) {
		t.Fatalf("tv: %v", got)
	}
	if got, _ := r.read("/films/A (2001)/A (2001).strm"); got != "http://site/d/media/films/A.mkv" {
		t.Errorf("first owner: %q", got)
	}
}

func TestFilesModeGetLinkKeepsSignOutOfThePathClean(t *testing.T) {
	d := &Strm{Addition: Addition{SiteUrl: "http://site/", PathPrefix: "/d", WithSign: true, EncodePath: true}}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	got := d.getLink(context.Background(), "/a//b/c d.mkv")
	if want := "http://site/d/a/b/c%20d.mkv?sign=" + sign.Sign("/a//b/c d.mkv"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
	d.WithSign = false
	if got, want := d.getLink(context.Background(), "/a/b.mkv"), "http://site/d/a/b.mkv"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	d.WithoutUrl = true
	if got, want := d.getLink(context.Background(), "/a/b.mkv"), "/d/a/b.mkv"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
