// Staged copy of our media handlers; their admin routes live under
// removed in step 4.11. Only what the library listing for users still needs is
// left (PLAN_MERGE_UPSTREAM_BE 2.3, 4.10).

package handles

import (
	"fmt"
	"net/url"
	pathpkg "path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

func mediaEnabled() bool { return MediaEnabled() }

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

func toListResp(it *model.MediaWork, rel func(string) string) MediaListResp {
	return MediaListResp{
		Path: rel(it.Path), Kind: it.Kind, Title: it.Title, Year: it.Year,
		Ratings:   unmarshalOr(it.Ratings, map[string]any{}),
		PosterURL: assetURL(it.PosterID, true),
	}
}

func toItemResp(it *model.MediaWork, rel func(string) string, withCandidates bool) *MediaItemResp {
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

	item, err := db.GetMediaWorkByPath(reqPath)
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	if item != nil {
		resp.Item = toItemResp(item, rel, user.IsAdmin())
		eps, err := db.GetMediaWorkEpisodes(item.ID)
		if err != nil {
			common.ErrorResp(c, err, 500, true)
			return
		}
		for _, e := range eps {
			resp.Episodes = append(resp.Episodes, MediaEpisodeResp{FileName: e.FileName, Season: e.Season, Episode: e.Episode, Title: e.Title, Overview: e.Overview})
		}
	}
	kids, err := db.ListMediaWorkChildren(reqPath)
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
func mediaItemVisible(user *model.User, item *model.MediaWork, parentMeta *model.Meta, password string) bool {
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
func collectionPosters(user *model.User, reqPath string, item *model.MediaWork, kids []model.MediaWork) (map[string]*[]string, error) {
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
	rows, err := db.ListMediaWorkPostersUnder(reqPath)
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
	items, total, err := db.ListMediaWorksFiltered(
		db.MediaWorkListFilter{Prefix: utils.FixAndCleanPath(user.BasePath), Kinds: kinds, Page: page, PerPage: per},
		func(it *model.MediaWork) bool {
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
