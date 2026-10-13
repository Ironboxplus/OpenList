package handles

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/media/apply"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/media/store"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// ---- wiring (overridable in tests) ----

// mediaAssetDir is where asset files live: <data>/media/assets.
var mediaAssetDir = func() string { return filepath.Join(flags.DataDir, "media", "assets") }

// mediaConfigHook lets tests point the providers at local servers.
var mediaConfigHook func(*scraper.Config)

// mediaAllowPrivateDownloads lets tests download from loopback.
var mediaAllowPrivateDownloads = false

func mediaStore() *store.Store { return store.New(mediaAssetDir()) }

func mediaEnabled() bool { return setting.GetBool(conf.MediaEnabled) }

func mediaScraperSet() (*scraper.Set, error) {
	cfg := scraper.Config{
		TMDBToken:    strings.TrimSpace(setting.GetStr(conf.MediaTMDBToken)),
		OMDbKeys:     scraper.SplitKeys(setting.GetStr(conf.MediaOMDbKeys)),
		DoubanAPIKey: strings.TrimSpace(setting.GetStr(conf.MediaDoubanAPIKey)),
		Language:     setting.GetStr(conf.MediaLanguage, "zh-CN"),
		Proxy:        strings.TrimSpace(setting.GetStr(conf.MediaHTTPProxy)),
		Timeout:      time.Duration(setting.GetInt(conf.MediaScraperTimeout, 15)) * time.Second,
	}
	if mediaConfigHook != nil {
		mediaConfigHook(&cfg)
	}
	return scraper.NewSet(cfg)
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
	switch {
	case errors.Is(err, apply.ErrLocked) || errors.Is(err, store.ErrLocked):
		common.ErrorStrResp(c, "this item is locked; unlock it or force the change", 409)
		return
	}
	if errors.Is(err, apply.ErrLibrary) {
		common.ErrorStrResp(c, err.Error(), 400)
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

// MediaItemResp is the detail view of an item.
type MediaItemResp struct {
	Path          string                 `json:"path"`
	Kind          string                 `json:"kind"`
	Title         string                 `json:"title"`
	OriginalTitle string                 `json:"original_title"`
	Year          int                    `json:"year"`
	ExternalIDs   map[string]string      `json:"external_ids"`
	Ratings       map[string]any         `json:"ratings"`
	Genres        []string               `json:"genres"`
	Overview      string                 `json:"overview"`
	PosterURL     string                 `json:"poster_url"`
	BackdropURL   string                 `json:"backdrop_url"`
	ChildPosters  *[]string              `json:"child_posters,omitempty"` // collections: up to 4 posters of what is inside
	Source        string                 `json:"source"`
	Confidence    float32                `json:"confidence"`
	Locked        bool                   `json:"locked"`
	Candidates    []model.MediaCandidate `json:"candidates"`
	ScrapedAt     *time.Time             `json:"scraped_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

// MediaListResp is the compact view used for children: no overview, no
// candidates, no episodes.
type MediaListResp struct {
	Path      string         `json:"path"`
	Kind      string         `json:"kind"`
	Title     string         `json:"title"`
	Year      int            `json:"year"`
	Ratings   map[string]any `json:"ratings"`
	PosterURL string         `json:"poster_url"`
	// ChildPosters is set for collections only: up to 4 signed thumb URLs of
	// the posters of the items below, by year then title.
	ChildPosters *[]string `json:"child_posters,omitempty"`
}

// MediaAdminListResp adds the review fields the admin list needs.
type MediaAdminListResp struct {
	MediaListResp
	Source         string  `json:"source"`
	Confidence     float32 `json:"confidence"`
	Locked         bool    `json:"locked"`
	CandidateCount int     `json:"candidate_count"`
}

// MediaEpisodeResp is one episode of the item.
type MediaEpisodeResp struct {
	FileName string `json:"file_name"`
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Title    string `json:"title"`
	Overview string `json:"overview"`
}

// MediaFsResp is the answer of /api/fs/media.
type MediaFsResp struct {
	Item     *MediaItemResp     `json:"item"`
	Children []MediaListResp    `json:"children"`
	Episodes []MediaEpisodeResp `json:"episodes"`
}

func emptyMediaFsResp() MediaFsResp {
	return MediaFsResp{Children: []MediaListResp{}, Episodes: []MediaEpisodeResp{}}
}

// assetURL is the signed URL of an asset: <img> can't send Authorization, so
// the signature rides in the query string.
func assetURL(id uint, thumb bool) string {
	if id == 0 {
		return ""
	}
	v := "orig"
	if thumb {
		v = "thumb"
	}
	prefix := ""
	if conf.URL != nil {
		prefix = strings.TrimRight(conf.URL.Path, "/")
	}
	s := sign.Sign(assetSignData(id))
	return fmt.Sprintf("%s/api/media/asset/%d?v=%s&sign=%s", prefix, id, v, url.QueryEscape(s))
}

func assetSignData(id uint) string { return "/media/asset/" + strconv.FormatUint(uint64(id), 10) }

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

func toListResp(it *model.MediaItem, rel func(string) string) MediaListResp {
	return MediaListResp{
		Path: rel(it.Path), Kind: it.Kind, Title: it.Title, Year: it.Year,
		Ratings:   unmarshalOr(it.Ratings, map[string]any{}),
		PosterURL: assetURL(it.PosterID, true),
	}
}

func toItemResp(it *model.MediaItem, rel func(string) string, withCandidates bool) *MediaItemResp {
	r := &MediaItemResp{
		Path: rel(it.Path), Kind: it.Kind, Title: it.Title, OriginalTitle: it.OriginalTitle, Year: it.Year,
		ExternalIDs: unmarshalOr(it.ExternalIDs, map[string]string{}),
		Ratings:     unmarshalOr(it.Ratings, map[string]any{}),
		Genres:      unmarshalOr(it.Genres, []string{}),
		Overview:    it.Overview,
		PosterURL:   assetURL(it.PosterID, false), BackdropURL: assetURL(it.BackdropID, false),
		Source: it.Source, Confidence: it.Confidence, Locked: it.Locked,
		Candidates: []model.MediaCandidate{},
		ScrapedAt:  it.ScrapedAt, UpdatedAt: it.UpdatedAt,
	}
	if withCandidates {
		r.Candidates = unmarshalOr(it.Candidates, []model.MediaCandidate{})
	}
	return r
}

func identityPath(p string) string { return p }

// userRelPath turns a stored absolute path into one relative to the user's
// base path, the way fs paths look to that user.
func userRelPath(user *model.User) func(string) string {
	base := utils.FixAndCleanPath(user.BasePath)
	if base == "/" {
		return identityPath
	}
	return func(p string) string {
		if p == base {
			return "/"
		}
		return strings.TrimPrefix(p, base)
	}
}

// ---- read API ----

// FsMedia answers /api/fs/media: the media item at a path, the items directly
// below it, and its episode map. It reads only the database. Anything the
// caller may not list (no access, hidden, wrong password, media off) comes
// back empty rather than as an error, so it says nothing about the path.
func FsMedia(c *gin.Context) {
	var req struct {
		Path     string `json:"path" form:"path"`
		Password string `json:"password" form:"password"`
	}
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	if user.IsGuest() && user.Disabled {
		common.ErrorStrResp(c, "Guest user is disabled, login please", 401)
		return
	}
	empty := emptyMediaFsResp()
	if strings.HasPrefix(req.Path, "/@s") || !mediaEnabled() {
		common.SuccessResp(c, empty)
		return
	}
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.SuccessResp(c, empty)
		return
	}
	meta, err := op.GetNearestMeta(reqPath)
	if err != nil && !errors.Is(errors.Cause(err), errs.MetaNotFound) {
		common.ErrorResp(c, err, 500, true)
		return
	}
	if !common.CanAccess(user, meta, reqPath, req.Password) {
		common.SuccessResp(c, empty)
		return
	}
	rel := userRelPath(user)
	resp := empty

	item, err := db.GetMediaItemByPath(reqPath)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	if item != nil {
		resp.Item = toItemResp(item, rel, user.IsAdmin())
		eps, err := db.GetMediaEpisodes(item.ID)
		if err != nil {
			common.ErrorResp(c, err, 500, true)
			return
		}
		for _, e := range eps {
			resp.Episodes = append(resp.Episodes, MediaEpisodeResp{FileName: e.FileName, Season: e.Season, Episode: e.Episode, Title: e.Title, Overview: e.Overview})
		}
	}
	kids, err := db.ListMediaChildren(reqPath)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	posters, err := collectionPosters(user, reqPath, item, kids)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	if resp.Item != nil && item.Kind == model.MediaKindCollection {
		resp.Item.ChildPosters = posters[""]
	}
	for i := range kids {
		k := &kids[i]
		if !mediaItemVisible(user, k, meta, req.Password) {
			continue
		}
		lr := toListResp(k, rel)
		if k.Kind == model.MediaKindCollection {
			lr.ChildPosters = posters[k.Path]
		}
		resp.Children = append(resp.Children, lr)
	}
	common.SuccessResp(c, resp)
}

// mediaItemVisible is the one rule for whether a user may see an item in a
// media listing: the hide patterns and password of the directory it sits in
// (parentMeta, the nearest meta of that directory), and the read restriction
// of its own nearest meta.
func mediaItemVisible(user *model.User, item *model.MediaItem, parentMeta *model.Meta, password string) bool {
	if !common.CanAccess(user, parentMeta, item.Path, password) {
		return false
	}
	cm, err := op.GetNearestMeta(item.Path)
	if err != nil && !errors.Is(errors.Cause(err), errs.MetaNotFound) {
		return false
	}
	return common.CanRead(user, cm, item.Path)
}

// collectionPosters finds up to 4 poster URLs for the collection at reqPath
// (key "") and for each collection among kids (key: its path). One query reads
// every poster below reqPath; the rest is grouping. An item the user may not
// read is skipped, so a poster never shows what the listing would hide.
func collectionPosters(user *model.User, reqPath string, item *model.MediaItem, kids []model.MediaItem) (map[string]*[]string, error) {
	out := map[string]*[]string{}
	var owners []string // the collections that want a mosaic
	if item != nil && item.Kind == model.MediaKindCollection {
		owners = append(owners, "")
	}
	for i := range kids {
		if kids[i].Kind == model.MediaKindCollection {
			owners = append(owners, kids[i].Path)
		}
	}
	if len(owners) == 0 {
		return out, nil
	}
	rows, err := db.ListMediaPostersUnder(reqPath)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		yi, yj := rows[i].Year, rows[j].Year
		if (yi == 0) != (yj == 0) {
			return yj == 0 // unknown years last
		}
		if yi != yj {
			return yi < yj
		}
		if rows[i].Title != rows[j].Title {
			return rows[i].Title < rows[j].Title
		}
		return rows[i].Path < rows[j].Path
	})
	readable := map[string]bool{}
	canRead := func(p string) bool {
		if v, ok := readable[p]; ok {
			return v
		}
		cm, err := op.GetNearestMeta(p)
		ok := (err == nil || errors.Is(errors.Cause(err), errs.MetaNotFound)) && common.CanRead(user, cm, p)
		readable[p] = ok
		return ok
	}
	for _, owner := range owners {
		urls := []string{}
		for i := range rows {
			if len(urls) == 4 {
				break
			}
			if owner != "" && !strings.HasPrefix(rows[i].Path, owner+"/") {
				continue
			}
			if !canRead(rows[i].Path) {
				continue
			}
			urls = append(urls, assetURL(rows[i].PosterID, true))
		}
		out[owner] = &urls
	}
	return out, nil
}

// libraryKinds are what the library lists when no kind is asked for; "ignore"
// is never listed, and "library" is only a root that opens as a poster wall.
var libraryKinds = []string{model.MediaKindMovie, model.MediaKindTV, model.MediaKindAnime, model.MediaKindCollection}

// MediaLibrary answers /api/media/library: the scraped items under the user's
// base path that the user may see, filtered before paging. Media off gives an
// empty list, like /api/fs/media.
func MediaLibrary(c *gin.Context) {
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	kinds := libraryKinds
	if k := c.Query("kind"); k != "" {
		if !slices.Contains(libraryKinds, k) {
			common.ErrorStrResp(c, "unsupported kind", 400)
			return
		}
		kinds = []string{k}
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	per, _ := strconv.Atoi(c.DefaultQuery("per_page", "100"))
	out := []MediaListResp{}
	if !mediaEnabled() {
		common.SuccessResp(c, gin.H{"content": out, "total": 0})
		return
	}
	parentMetas := map[string]*model.Meta{}
	var metaErr error
	parentMeta := func(dir string) *model.Meta {
		if m, ok := parentMetas[dir]; ok {
			return m
		}
		m, err := op.GetNearestMeta(dir)
		if err != nil && !errors.Is(errors.Cause(err), errs.MetaNotFound) {
			metaErr, m = err, nil
		}
		parentMetas[dir] = m
		return m
	}
	items, total, err := db.ListMediaItemsFiltered(
		db.MediaListFilter{Prefix: utils.FixAndCleanPath(user.BasePath), Kinds: kinds, Page: page, PerPage: per},
		func(it *model.MediaItem) bool {
			return mediaItemVisible(user, it, parentMeta(pathpkg.Dir(it.Path)), "")
		})
	if err == nil {
		err = metaErr
	}
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	rel := userRelPath(user)
	for i := range items {
		out = append(out, toListResp(&items[i], rel))
	}
	common.SuccessResp(c, gin.H{"content": out, "total": total})
}

// assetFail answers with a real HTTP status: an <img> only sees the status,
// not the envelope code.
func assetFail(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, common.Resp[any]{Code: status, Message: msg})
}

// MediaAsset serves an image. The signature is checked over
// "/media/asset/<id>", because an <img> tag can't carry a token.
func MediaAsset(c *gin.Context) {
	id64, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id64 == 0 {
		assetFail(c, http.StatusBadRequest, "bad asset id")
		return
	}
	id := uint(id64)
	if err := sign.Verify(assetSignData(id), c.Query("sign")); err != nil {
		assetFail(c, http.StatusForbidden, "invalid or expired sign")
		return
	}
	a, err := db.GetMediaAsset(id)
	if err != nil {
		assetFail(c, http.StatusInternalServerError, "database error")
		return
	}
	if a == nil {
		assetFail(c, http.StatusNotFound, "asset not found")
		return
	}
	thumb := c.DefaultQuery("v", "orig") == "thumb"
	st := mediaStore()
	f, err := os.Open(st.AssetFile(a, thumb))
	if err != nil && thumb {
		f, err = os.Open(st.AssetFile(a, false)) // thumbnail missing: fall back to the original
		thumb = false
	}
	if err != nil {
		assetFail(c, http.StatusNotFound, "asset file missing")
		return
	}
	defer f.Close()
	etag := `"` + a.SHA256 + `"`
	mime := a.Mime
	if thumb {
		etag = `"` + a.SHA256 + `-thumb"`
		mime = "image/jpeg"
	}
	h := c.Writer.Header()
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", etag)
	h.Set("Content-Type", mime)
	http.ServeContent(c.Writer, c.Request, "", a.CreatedAt, f)
}

// ---- admin API ----

// MediaSet upserts an item; only the fields in the request are written.
func MediaSet(c *gin.Context) {
	var req model.MediaSetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	for _, idp := range []*uint{req.PosterID, req.BackdropID} {
		if idp != nil && *idp != 0 {
			if a, err := db.GetMediaAsset(*idp); err != nil || a == nil {
				common.ErrorStrResp(c, fmt.Sprintf("asset %d does not exist", *idp), 400)
				return
			}
		}
	}
	it, err := mediaStore().Set(req, store.SetOptions{})
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, toItemResp(it, identityPath, true))
}

// MediaDelete removes one item, or with recursive a whole subtree.
func MediaDelete(c *gin.Context) {
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

// MediaAdminList pages through items for review.
func MediaAdminList(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	per, _ := strconv.Atoi(c.DefaultQuery("per_page", "100"))
	f := db.MediaListFilter{
		Prefix: c.Query("prefix"), Kind: c.Query("kind"),
		Pending: c.Query("pending") == "true" || c.Query("pending") == "1",
		Page:    page, PerPage: per,
	}
	if f.Prefix != "" {
		f.Prefix = store.NormalizePath(f.Prefix)
	}
	items, total, err := db.ListMediaItems(f)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	out := make([]MediaAdminListResp, 0, len(items))
	for i := range items {
		it := &items[i]
		out = append(out, MediaAdminListResp{
			MediaListResp: toListResp(it, identityPath),
			Source:        it.Source, Confidence: it.Confidence, Locked: it.Locked,
			CandidateCount: len(unmarshalOr(it.Candidates, []model.MediaCandidate{})),
		})
	}
	common.SuccessResp(c, gin.H{"content": out, "total": total})
}

// MediaAssetUpload stores an image: a multipart "file", or a JSON {url} the
// server downloads (through media_http_proxy). Same bytes, same asset.
func MediaAssetUpload(c *gin.Context) {
	st := mediaStore()
	var data []byte
	var src string
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
		data, err = io.ReadAll(io.LimitReader(f, apply.MaxImageBytes+1))
		if err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		src = c.Query("source_url")
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
		data, err = dl(c.Request.Context(), req.URL)
		if err != nil {
			mediaErr(c, err)
			return
		}
		src = req.URL
	}
	a, err := st.AddAsset(data, src)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	common.SuccessResp(c, gin.H{"id": a.ID, "w": a.Width, "h": a.Height})
}

// MediaScrape scrapes one path on the server. With an id it fetches that
// item; without, it identifies and searches. A locked item is left alone.
func MediaScrape(c *gin.Context) {
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
	if !mediaEnabled() {
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

// MediaSearch searches one provider for the manual-pick UI.
func MediaSearch(c *gin.Context) {
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

// ---- export / import ----

type mediaExportAsset struct {
	SHA256    string `json:"sha256"`
	Mime      string `json:"mime"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	SourceURL string `json:"source_url"`
}

type mediaExportItem struct {
	Path          string                 `json:"path"`
	Kind          string                 `json:"kind"`
	Title         string                 `json:"title"`
	OriginalTitle string                 `json:"original_title"`
	Year          int                    `json:"year"`
	ExternalIDs   map[string]string      `json:"external_ids"`
	Ratings       map[string]any         `json:"ratings"`
	Genres        []string               `json:"genres"`
	Overview      string                 `json:"overview"`
	PosterSHA     string                 `json:"poster_sha,omitempty"`
	BackdropSHA   string                 `json:"backdrop_sha,omitempty"`
	Source        string                 `json:"source"`
	Confidence    float32                `json:"confidence"`
	Locked        bool                   `json:"locked"`
	Candidates    []model.MediaCandidate `json:"candidates"`
	Episodes      []model.MediaEpisodeIn `json:"episodes"`
}

type mediaExportDoc struct {
	Version    int                `json:"version"`
	ExportedAt time.Time          `json:"exported_at"`
	Items      []mediaExportItem  `json:"items"`
	Assets     []mediaExportAsset `json:"assets"`
}

// MediaExport writes every item (optionally under ?prefix=) as one JSON
// document. Images are referenced by SHA-256, not embedded.
func MediaExport(c *gin.Context) {
	prefix := c.Query("prefix")
	if prefix != "" {
		prefix = store.NormalizePath(prefix)
	}
	items, err := db.AllMediaItems(prefix)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	var ids []uint
	for _, it := range items {
		ids = append(ids, it.PosterID, it.BackdropID)
	}
	assets, err := db.GetMediaAssets(nonZero(ids))
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	shaByID := map[uint]string{}
	doc := mediaExportDoc{Version: 1, ExportedAt: time.Now().UTC(), Items: []mediaExportItem{}, Assets: []mediaExportAsset{}}
	for _, a := range assets {
		shaByID[a.ID] = a.SHA256
		doc.Assets = append(doc.Assets, mediaExportAsset{SHA256: a.SHA256, Mime: a.Mime, Width: a.Width, Height: a.Height, SourceURL: a.SourceURL})
	}
	for _, it := range items {
		eps, err := db.GetMediaEpisodes(it.ID)
		if err != nil {
			common.ErrorResp(c, err, 500, true)
			return
		}
		e := mediaExportItem{
			Path: it.Path, Kind: it.Kind, Title: it.Title, OriginalTitle: it.OriginalTitle, Year: it.Year,
			ExternalIDs: unmarshalOr(it.ExternalIDs, map[string]string{}), Ratings: unmarshalOr(it.Ratings, map[string]any{}),
			Genres: unmarshalOr(it.Genres, []string{}), Overview: it.Overview,
			PosterSHA: shaByID[it.PosterID], BackdropSHA: shaByID[it.BackdropID],
			Source: it.Source, Confidence: it.Confidence, Locked: it.Locked,
			Candidates: unmarshalOr(it.Candidates, []model.MediaCandidate{}), Episodes: []model.MediaEpisodeIn{},
		}
		for _, ep := range eps {
			e.Episodes = append(e.Episodes, model.MediaEpisodeIn{FileName: ep.FileName, Season: ep.Season, Episode: ep.Episode, Title: ep.Title, Overview: ep.Overview})
		}
		doc.Items = append(doc.Items, e)
	}
	c.Header("Content-Disposition", `attachment; filename="openlist-media.json"`)
	c.JSON(http.StatusOK, doc)
}

func nonZero(ids []uint) []uint {
	seen := map[uint]bool{}
	var out []uint
	for _, id := range ids {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// MediaImport loads an export. Assets it lacks are downloaded again from their
// recorded source URL; a locked item is kept unless ?overwrite_locked=true.
func MediaImport(c *gin.Context) {
	var doc mediaExportDoc
	if err := c.ShouldBindJSON(&doc); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if doc.Version != 1 {
		common.ErrorStrResp(c, fmt.Sprintf("unsupported export version %d", doc.Version), 400)
		return
	}
	overwriteLocked := c.Query("overwrite_locked") == "true"
	st := mediaStore()
	dl, err := mediaDownloader()
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}

	idBySHA := map[string]uint{}
	var downloaded, missing int
	for _, a := range doc.Assets {
		if have, err := db.GetMediaAssetBySHA(a.SHA256); err == nil && have != nil {
			idBySHA[a.SHA256] = have.ID
			continue
		}
		if a.SourceURL == "" {
			missing++
			continue
		}
		data, err := dl(c.Request.Context(), a.SourceURL)
		if err != nil {
			missing++
			continue
		}
		na, err := st.AddAsset(data, a.SourceURL)
		if err != nil {
			missing++
			continue
		}
		idBySHA[a.SHA256] = na.ID
		downloaded++
	}

	var imported, skipped int
	for _, it := range doc.Items {
		kind, title, overview, src := it.Kind, it.Title, it.Overview, it.Source
		if src == "" {
			src = model.MediaSourceManual
		}
		year, conf, locked, orig := it.Year, it.Confidence, it.Locked, it.OriginalTitle
		poster, backdrop := idBySHA[it.PosterSHA], idBySHA[it.BackdropSHA]
		req := model.MediaSetReq{
			Path: it.Path, Kind: &kind, Title: &title, OriginalTitle: &orig, Year: &year,
			ExternalIDs: nonNilMap(it.ExternalIDs), Ratings: nonNilAny(it.Ratings), Genres: nonNilSlice(it.Genres),
			Overview: &overview, PosterID: &poster, BackdropID: &backdrop,
			Locked: &locked, Source: &src, Confidence: &conf,
			Candidates: nonNilCands(it.Candidates), Episodes: it.Episodes,
		}
		if req.Episodes == nil {
			req.Episodes = []model.MediaEpisodeIn{}
		}
		_, err := st.Set(req, store.SetOptions{RespectLock: !overwriteLocked})
		switch {
		case errors.Is(err, store.ErrLocked):
			skipped++
		case err != nil:
			common.ErrorStrResp(c, fmt.Sprintf("%s: %v", it.Path, err), 400)
			return
		default:
			imported++
		}
	}
	common.SuccessResp(c, gin.H{"imported": imported, "skipped_locked": skipped, "assets_downloaded": downloaded, "assets_missing": missing})
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
func nonNilAny(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
func nonNilCands(s []model.MediaCandidate) []model.MediaCandidate {
	if s == nil {
		return []model.MediaCandidate{}
	}
	return s
}
