package handles

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"io"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/media/apply"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/media/store"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// The curate API is what `openlist media` talks to (and what an admin UI can
// use to correct a match): per-path reads and writes of the curation fields of
// a video work, a scrape of one path, a provider search and image upload. It
// sits under /api/admin/media/curate and is admin only. The library as a whole
// (scan, scrape-all, listing for users) is the upstream API.

// ---- wiring (overridable in tests) ----

// mediaConfigHook lets tests point the providers at local servers.
var mediaConfigHook func(*scraper.Config)

// mediaAllowPrivateDownloads lets tests download from loopback.
var mediaAllowPrivateDownloads = false

// mediaStore keeps works and images the way the library is configured to keep
// covers.
func mediaStore() *store.Store {
	return store.New(setting.GetStr(conf.MediaThumbnailMode, "runtime"), setting.GetStr(conf.MediaThumbnailPath, "/imgs"))
}

// MediaScraperConfig is the provider configuration the server scrapes with, built
// from the media settings. `openlist media record` uses it too, so a recording
// sees what a scrape sees.
func MediaScraperConfig() scraper.Config {
	cfg := scraper.Config{
		TMDBToken:    strings.TrimSpace(setting.GetStr(conf.MediaTMDBKey)),
		OMDbKeys:     scraper.SplitKeys(setting.GetStr(conf.MediaOMDbKeys)),
		DoubanAPIKey: strings.TrimSpace(setting.GetStr(conf.MediaDoubanAPIKey)),
		Language:     setting.GetStr(conf.MediaScrapeLanguage, "zh-CN"),
		Proxy:        strings.TrimSpace(setting.GetStr(conf.MediaHTTPProxy)),
		Timeout:      time.Duration(setting.GetInt(conf.MediaScraperTimeout, 15)) * time.Second,
	}
	if u := tmdbBaseURL(setting.GetStr(conf.MediaTMDBAPIURL)); u != "" {
		cfg.BaseURLs = map[string]string{"tmdb": u}
	}
	return cfg
}

func mediaScraperSet() (*scraper.Set, error) {
	cfg := MediaScraperConfig()
	if mediaConfigHook != nil {
		mediaConfigHook(&cfg)
	}
	return scraper.NewSet(cfg)
}

// tmdbBaseURL turns the media_tmdb_api_url setting into the TMDB base URL with
// its /3 prefix. It takes a bare host (api.themoviedb.org), a reverse proxy
// prefix (https://example.com/api/tmdb) or the full form already ending in /3.
// Empty and the official host give "" (the provider default).
func tmdbBaseURL(v string) string {
	base := strings.TrimRight(strings.TrimSpace(v), "/")
	if base == "" {
		return ""
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	if !strings.HasSuffix(base, "/3") {
		base += "/3"
	}
	if base == "https://api.themoviedb.org/3" {
		return ""
	}
	return base
}

func mediaDownloader() (apply.Downloader, error) {
	return apply.NewDownloader(apply.DownloadOpts{
		Proxy:        strings.TrimSpace(setting.GetStr(conf.MediaHTTPProxy)),
		Timeout:      time.Duration(setting.GetInt(conf.MediaScraperTimeout, 15)) * time.Second,
		AllowPrivate: mediaAllowPrivateDownloads,
	})
}

// mediaErr answers a failed provider call. A server that can't reach the
// provider says so as class "network", which the UI turns into "use the CLI
// on a PC".
func mediaErr(c *gin.Context, err error) {
	if errors.Is(err, apply.ErrLocked) || errors.Is(err, store.ErrLocked) {
		common.ErrorStrResp(c, "this item is locked; unlock it or force the change", 409)
		return
	}
	class := scraper.ClassOf(err)
	switch class {
	case scraper.ClassNetwork:
		common.ErrorWithDataResp(c, fmt.Errorf("network: %s", err.Error()), 502, gin.H{"class": class}, true)
	case scraper.ClassAuth, scraper.ClassRate, scraper.ClassNotConfig:
		common.ErrorWithDataResp(c, fmt.Errorf("%s: %s", class, err.Error()), 502, gin.H{"class": class}, true)
	case scraper.ClassNotFound:
		common.ErrorWithDataResp(c, err, 404, gin.H{"class": class})
	default:
		common.ErrorWithDataResp(c, err, 500, gin.H{"class": class}, true)
	}
}

// ---- DTOs ----

// CurateItemResp is a work as the curate API shows it.
type CurateItemResp struct {
	Path           string                 `json:"path"`
	Kind           string                 `json:"kind"`
	Title          string                 `json:"title"`
	OriginalTitle  string                 `json:"original_title"`
	Year           int                    `json:"year"`
	ExternalIDs    map[string]string      `json:"external_ids"`
	Ratings        map[string]any         `json:"ratings"`
	Source         string                 `json:"source"`
	Confidence     float32                `json:"confidence"`
	Locked         bool                   `json:"locked"`
	CandidateCount int                    `json:"candidate_count"`
	Candidates     []model.MediaCandidate `json:"candidates,omitempty"`
	Cover          string                 `json:"cover,omitempty"`
	Backdrop       string                 `json:"backdrop,omitempty"`
}

// mediaYear is the year of a release date ("2003-01-01"), 0 when unknown.
func mediaYear(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(date[:4])
	return y
}

func curateItem(it *model.MediaItem, ratings []model.MediaRating, withCandidates bool) CurateItemResp {
	r := CurateItemResp{
		Path: db.MediaItemPath(it), Kind: it.VideoType, Title: it.ScrapedName, OriginalTitle: it.OriginalTitle,
		Year:        mediaYear(it.ReleaseDate),
		ExternalIDs: unmarshalOr(it.ExternalIDs, map[string]string{}),
		Ratings:     store.RatingsToMap(ratings),
		Source:      it.Source, Confidence: it.Confidence, Locked: it.Locked,
		Cover: it.Cover, Backdrop: it.Backdrop,
	}
	cands := unmarshalOr(it.Candidates, []model.MediaCandidate{})
	r.CandidateCount = len(cands)
	if withCandidates {
		r.Candidates = cands
	}
	return r
}

func unmarshalOr[T any](s string, def T) T {
	if s == "" {
		return def
	}
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return def
	}
	if rv := reflect.ValueOf(v); (rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice) && rv.IsNil() {
		return def
	}
	return v
}

// ---- handlers ----

// CurateSet upserts a work; only the fields in the request are written.
func CurateSet(c *gin.Context) {
	var req model.MediaSetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	it, err := mediaStore().Set(req, store.SetOptions{})
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	rs, err := db.ListMediaRatings(it.ID)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, curateItem(it, rs[it.ID], true))
}

// CurateDelete removes one work, or with recursive a whole subtree.
func CurateDelete(c *gin.Context) {
	var req struct {
		Path      string `json:"path"`
		Prefix    string `json:"prefix"`
		Recursive bool   `json:"recursive"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	var n int64
	var err error
	switch {
	case req.Recursive && strings.TrimSpace(req.Prefix) != "":
		if store.NormalizePath(req.Prefix) == "/" {
			common.ErrorStrResp(c, "refusing to delete every media item; give a narrower prefix", 400)
			return
		}
		n, err = mediaStore().DeletePrefix(req.Prefix)
	case strings.TrimSpace(req.Path) != "":
		n, err = mediaStore().Delete(req.Path)
	default:
		common.ErrorStrResp(c, "give a path, or a prefix with recursive=true", 400)
		return
	}
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, gin.H{"deleted": n})
}

// CurateList pages through works for review.
func CurateList(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	per, _ := strconv.Atoi(c.DefaultQuery("per_page", "100"))
	f := db.VideoListFilter{
		Prefix:  store.NormalizePath(c.DefaultQuery("prefix", "/")),
		Pending: c.Query("pending") == "true" || c.Query("pending") == "1",
		Page:    page, PerPage: per,
	}
	if k := c.Query("kind"); k != "" {
		if !model.ValidMediaKind(k) {
			common.ErrorStrResp(c, "unsupported kind", 400)
			return
		}
		f.Kinds = []string{k}
	}
	items, total, err := db.ListVideoItems(f)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	ids := make([]uint, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
	}
	rs, err := db.ListMediaRatings(ids...)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	out := make([]CurateItemResp, 0, len(items))
	for i := range items {
		out = append(out, curateItem(&items[i], rs[items[i].ID], false))
	}
	common.SuccessResp(c, gin.H{"content": out, "total": total})
}

// CurateImage stores a poster or backdrop of the work at ?path= (?role=poster
// or backdrop) and answers with the cover reference. The body is a multipart
// "file", or JSON {url} the server downloads (through media_http_proxy). It
// does not touch the work: the caller sets the reference with CurateSet.
func CurateImage(c *gin.Context) {
	p := strings.TrimSpace(c.Query("path"))
	role := c.DefaultQuery("role", "poster")
	if p == "" || (role != "poster" && role != "backdrop") {
		common.ErrorStrResp(c, "path and role (poster or backdrop) are required", 400)
		return
	}
	var data []byte
	if strings.HasPrefix(c.ContentType(), "multipart/") {
		fh, err := c.FormFile("file")
		if err != nil {
			common.ErrorStrResp(c, "multipart field \"file\" is required", 400)
			return
		}
		f, err := fh.Open()
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		defer f.Close()
		if data, err = io.ReadAll(io.LimitReader(f, store.MaxImageBytes+1)); err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
	} else {
		var req struct {
			URL string `json:"url"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.URL) == "" {
			common.ErrorStrResp(c, "send a multipart file or {\"url\": ...}", 400)
			return
		}
		dl, err := mediaDownloader()
		if err != nil {
			common.ErrorResp(c, err, 500, true)
			return
		}
		if data, err = dl(c.Request.Context(), req.URL); err != nil {
			mediaErr(c, err)
			return
		}
	}
	cover, err := mediaStore().SaveImage(c.Request.Context(), p, role, data)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, gin.H{"cover": cover})
}

// CurateScrape scrapes one path on the server. With an id it fetches that
// item; without, it identifies and searches. A locked item is left alone.
func CurateScrape(c *gin.Context) {
	var req struct {
		Path     string `json:"path"`
		Provider string `json:"provider"`
		ID       string `json:"id"`
		Lock     bool   `json:"lock"`
		Force    bool   `json:"force"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		common.ErrorStrResp(c, "path is required", 400)
		return
	}
	if !MediaEnabled() {
		common.ErrorStrResp(c, "media scraping is disabled (media_enabled)", 400)
		return
	}
	set, err := mediaScraperSet()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	dl, err := mediaDownloader()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	o := &apply.Orchestrator{
		Lister: apply.LocalLister{}, Writer: apply.LocalWriter{Store: mediaStore()},
		Providers: set, Download: dl,
	}
	res, err := o.ScrapeOne(c.Request.Context(), req.Path, apply.ScrapeOpts{Provider: req.Provider, ID: req.ID, Lock: req.Lock, Force: req.Force})
	if err != nil {
		mediaErr(c, err)
		return
	}
	common.SuccessResp(c, res)
}

// CurateSearch searches one provider for the manual-pick UI.
func CurateSearch(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		common.ErrorStrResp(c, "q is required", 400)
		return
	}
	year, _ := strconv.Atoi(c.Query("year"))
	set, err := mediaScraperSet()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	cs, err := set.Search(c.Request.Context(), c.DefaultQuery("provider", "tmdb"), scraper.SearchQuery{Q: q, Kind: c.Query("kind"), Year: year})
	if err != nil {
		mediaErr(c, err)
		return
	}
	if cs == nil {
		cs = []scraper.Candidate{}
	}
	common.SuccessResp(c, cs)
}

// CurateExportNFO writes an NFO next to the works a person fixed (locked, or
// picked by hand) so another player or a rebuilt library reads the same match.
// Without "apply" it only reports what it would write; it never overwrites an
// NFO, and uploads one file at a time.
func CurateExportNFO(c *gin.Context) {
	var req struct {
		Paths []string `json:"paths"`
		Apply bool     `json:"apply"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Paths) == 0 {
		common.ErrorStrResp(c, "paths is required", 400)
		return
	}
	var items []model.MediaItem
	for _, p := range req.Paths {
		found, err := db.ListVideoItemsUnder(path.Clean("/" + strings.TrimLeft(strings.ReplaceAll(p, "\\", "/"), "/")))
		if err != nil {
			common.ErrorResp(c, err, 500, true)
			return
		}
		items = append(items, found...)
	}
	put := func(ctx context.Context, dir, name string, data []byte) error {
		return fs.PutDirectly(ctx, dir, &stream.FileStream{
			Obj:      &model.Object{Name: name, Size: int64(len(data)), Modified: time.Now()},
			Reader:   bytes.NewReader(data),
			Mimetype: "text/xml",
		})
	}
	common.SuccessResp(c, apply.ExportNFO(c.Request.Context(), apply.LocalLister{}, put, items, req.Apply))
}
