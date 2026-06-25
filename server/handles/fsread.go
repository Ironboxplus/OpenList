package handles

import (
	"fmt"
	stdpath "path"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/plugin"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

type ListReq struct {
	model.PageReq
	Path     string `json:"path" form:"path"`
	Password string `json:"password" form:"password"`
	Refresh  bool   `json:"refresh"`
}

type DirReq struct {
	Path      string `json:"path" form:"path"`
	Password  string `json:"password" form:"password"`
	ForceRoot bool   `json:"force_root" form:"force_root"`
}

type ObjResp struct {
	Name         string                     `json:"name"`
	Size         int64                      `json:"size"`
	IsDir        bool                       `json:"is_dir"`
	Modified     time.Time                  `json:"modified"`
	Created      time.Time                  `json:"created"`
	Sign         string                     `json:"sign"`
	Thumb        string                     `json:"thumb"`
	Type         int                        `json:"type"`
	HashInfoStr  string                     `json:"hashinfo"`
	HashInfo     map[*utils.HashType]string `json:"hash_info"`
	MountDetails *model.StorageDetails      `json:"mount_details,omitempty"`
	// Extra carries optional driver-specific metadata (e.g. media duration,
	// video resolution, starred). Clients render known keys and ignore the rest.
	Extra map[string]any `json:"extra,omitempty"`
}

type FsListResp struct {
	Content            []ObjResp `json:"content"`
	Total              int64     `json:"total"`
	Readme             string    `json:"readme"`
	Header             string    `json:"header"`
	Write              bool      `json:"write"`
	WriteContentBypass bool      `json:"write_content_bypass"`
	Provider           string    `json:"provider"`
	DirectUploadTools  []string  `json:"direct_upload_tools,omitempty"`
	// MountDetails is the disk usage of the storage that the *current directory*
	// belongs to (nil at the virtual storages-root or when hidden). It lets the
	// web header show the current mount's usage dynamically on folder navigation
	// without firing an extra fs/get per click.
	MountDetails *model.StorageDetails `json:"mount_details,omitempty"`
}

func FsListSplit(c *gin.Context) {
	var req ListReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	req.Validate()
	if strings.HasPrefix(req.Path, "/@s") {
		req.Path = strings.TrimPrefix(req.Path, "/@s")
		SharingList(c, &req)
		return
	}
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	if user.IsGuest() && user.Disabled {
		common.ErrorStrResp(c, "Guest user is disabled, login please", 401)
		return
	}
	FsList(c, &req, user)
}

func FsList(c *gin.Context, req *ListReq, user *model.User) {
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	meta, err := op.GetNearestMeta(reqPath)
	if err != nil && !errors.Is(errors.Cause(err), errs.MetaNotFound) {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.GinAppendValues(c, conf.MetaKey, meta)
	if !common.CanAccess(user, meta, reqPath, req.Password) {
		common.ErrorStrResp(c, "password is incorrect or you have no permission", 403)
		return
	}
	canWriteContentAtPath := common.CanWrite(user, meta, reqPath) && (user.CanWriteContent() || common.CanWriteContentBypassUserPerms(meta, reqPath))
	if req.Refresh && !canWriteContentAtPath {
		common.ErrorStrResp(c, "Refresh without permission", 403)
		return
	}
	objs, err := fs.List(c.Request.Context(), reqPath, &fs.ListArgs{
		Refresh:            req.Refresh,
		WithStorageDetails: !user.IsGuest() && !setting.GetBool(conf.HideStorageDetails),
	})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	total, objs := pagination(objs, &req.PageReq)
	provider := "unknown"
	var directUploadTools []string
	var mountDetails *model.StorageDetails
	if storage, err := fs.GetStorage(reqPath, &fs.GetStoragesArgs{}); err == nil {
		if canWriteContentAtPath {
			directUploadTools = op.GetDirectUploadTools(storage)
		}
		// Current directory's storage usage for the web header. Cache-backed +
		// singleflight in op.GetStorageDetails, so concurrent multi-user access
		// collapses to at most one provider call per storage per TTL — no 429.
		if !user.IsGuest() && !setting.GetBool(conf.HideStorageDetails) {
			if d, e := op.GetStorageDetails(c.Request.Context(), storage); e == nil {
				mountDetails = d
			}
		}
	}
	common.SuccessResp(c, FsListResp{
		Content:            toObjsResp(objs, reqPath, isEncrypt(meta, reqPath)),
		Total:              int64(total),
		Readme:             getReadme(meta, reqPath),
		Header:             getHeader(meta, reqPath),
		Write:              common.CanWrite(user, meta, reqPath),
		WriteContentBypass: common.CanWriteContentBypassUserPerms(meta, reqPath),
		Provider:           provider,
		DirectUploadTools:  directUploadTools,
		MountDetails:       mountDetails,
	})
	if plugin.HasSubscribers(plugin.HookFsListAfter) {
		plugin.FireHook(plugin.HookFsListAfter, map[string]any{
			"path":  reqPath,
			"count": total,
		})
	}
}

func FsDirs(c *gin.Context) {
	var req DirReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	reqPath := req.Path
	if req.ForceRoot {
		if !user.IsAdmin() {
			common.ErrorStrResp(c, "Permission denied", 403)
			return
		}
	} else {
		tmp, err := user.JoinPath(req.Path)
		if err != nil {
			common.ErrorResp(c, err, 403)
			return
		}
		reqPath = tmp
	}
	meta, err := op.GetNearestMeta(reqPath)
	if err != nil && !errors.Is(errors.Cause(err), errs.MetaNotFound) {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.GinAppendValues(c, conf.MetaKey, meta)
	if !common.CanAccess(user, meta, reqPath, req.Password) {
		common.ErrorStrResp(c, "password is incorrect or you have no permission", 403)
		return
	}
	objs, err := fs.List(c.Request.Context(), reqPath, &fs.ListArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	dirs := filterDirs(objs)
	common.SuccessResp(c, dirs)
}

type DirResp struct {
	Name     string    `json:"name"`
	Modified time.Time `json:"modified"`
}

func filterDirs(objs []model.Obj) []DirResp {
	var dirs []DirResp
	for _, obj := range objs {
		if obj.IsDir() {
			dirs = append(dirs, DirResp{
				Name:     obj.GetName(),
				Modified: obj.ModTime(),
			})
		}
	}
	return dirs
}

func getReadme(meta *model.Meta, path string) string {
	if meta != nil && common.MetaCoversPath(meta.Path, path, meta.RSub) {
		return meta.Readme
	}
	return ""
}

func getHeader(meta *model.Meta, path string) string {
	if meta != nil && common.MetaCoversPath(meta.Path, path, meta.HeaderSub) {
		return meta.Header
	}
	return ""
}

func isEncrypt(meta *model.Meta, path string) bool {
	if common.IsStorageSignEnabled(path) {
		return true
	}
	if meta == nil || meta.Password == "" {
		return false
	}
	if !common.MetaCoversPath(meta.Path, path, meta.PSub) {
		return false
	}
	return true
}

func pagination(objs []model.Obj, req *model.PageReq) (int, []model.Obj) {
	pageIndex, pageSize := req.Page, req.PerPage
	total := len(objs)
	start := (pageIndex - 1) * pageSize
	if start > total {
		return total, []model.Obj{}
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return total, objs[start:end]
}

func toObjsResp(objs []model.Obj, parent string, encrypt bool) []ObjResp {
	var resp []ObjResp
	for _, obj := range objs {
		thumb, _ := model.GetThumb(obj)
		mountDetails, _ := model.GetStorageDetails(obj)
		extra, _ := model.GetExtra(obj)
		hashInfo := obj.GetHash().Export()
		if hashInfo == nil {
			hashInfo = make(map[*utils.HashType]string)
		}
		resp = append(resp, ObjResp{
			Name:         obj.GetName(),
			Size:         obj.GetSize(),
			IsDir:        obj.IsDir(),
			Modified:     obj.ModTime(),
			Created:      obj.CreateTime(),
			HashInfoStr:  obj.GetHash().String(),
			HashInfo:     hashInfo,
			Sign:         common.Sign(obj, parent, encrypt),
			Thumb:        thumb,
			Type:         utils.GetObjType(obj.GetName(), obj.IsDir()),
			MountDetails: mountDetails,
			Extra:        extra,
		})
	}
	return resp
}

type FsGetReq struct {
	Path     string `json:"path" form:"path"`
	Password string `json:"password" form:"password"`
}

type FsGetResp struct {
	ObjResp
	RawURL   string    `json:"raw_url"`
	Readme   string    `json:"readme"`
	Header   string    `json:"header"`
	Provider string    `json:"provider"`
	Related  []ObjResp `json:"related"`
}

func FsGetSplit(c *gin.Context) {
	var req FsGetReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if strings.HasPrefix(req.Path, "/@s") {
		req.Path = strings.TrimPrefix(req.Path, "/@s")
		SharingGet(c, &req)
		return
	}
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	if user.IsGuest() && user.Disabled {
		common.ErrorStrResp(c, "Guest user is disabled, login please", 401)
		return
	}
	FsGet(c, &req, user)
}

func FsGet(c *gin.Context, req *FsGetReq, user *model.User) {
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	meta, err := op.GetNearestMeta(reqPath)
	if err != nil && !errors.Is(errors.Cause(err), errs.MetaNotFound) {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.GinAppendValues(c, conf.MetaKey, meta)
	if !common.CanAccess(user, meta, reqPath, req.Password) {
		common.ErrorStrResp(c, "password is incorrect or you have no permission", 403)
		return
	}
	obj, err := fs.Get(c.Request.Context(), reqPath, &fs.GetArgs{
		WithStorageDetails: !user.IsGuest() && !setting.GetBool(conf.HideStorageDetails),
	})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	var rawURL string

	storage, err := fs.GetStorage(reqPath, &fs.GetStoragesArgs{})
	provider, ok := model.GetProvider(obj)
	if !ok && err == nil {
		provider = storage.Config().Name
	}
	if !obj.IsDir() {
		if err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
		if storage.Config().MustProxy() || storage.GetStorage().WebProxy {
			rawURL = common.GenerateDownProxyURL(storage.GetStorage(), reqPath)
			if rawURL == "" {
				query := ""
				if isEncrypt(meta, reqPath) || setting.GetBool(conf.SignAll) {
					query = "?sign=" + sign.Sign(reqPath)
				}
				rawURL = fmt.Sprintf("%s/p%s%s",
					common.GetApiUrl(c),
					utils.EncodePath(reqPath, true),
					query)
			}
		} else {
			// file have raw url
			if url, ok := model.GetUrl(obj); ok {
				rawURL = url
			} else {
				// if storage is not proxy, use raw url by fs.Link
				link, _, err := fs.Link(c.Request.Context(), reqPath, model.LinkArgs{
					IP:       c.ClientIP(),
					Header:   c.Request.Header,
					Redirect: true,
				})
				if err != nil {
					common.ErrorResp(c, err, 500)
					return
				}
				defer link.Close()
				rawURL = link.URL
			}
		}
	}
	var related []model.Obj
	parentPath := stdpath.Dir(reqPath)
	sameLevelFiles, err := fs.List(c.Request.Context(), parentPath, &fs.ListArgs{})
	if err == nil {
		related = filterRelated(sameLevelFiles, obj)
	}
	parentMeta, _ := op.GetNearestMeta(parentPath)
	thumb, _ := model.GetThumb(obj)
	mountDetails, _ := model.GetStorageDetails(obj)
	extra, _ := model.GetExtra(obj)
	common.SuccessResp(c, FsGetResp{
		ObjResp: ObjResp{
			Name:         obj.GetName(),
			Size:         obj.GetSize(),
			IsDir:        obj.IsDir(),
			Modified:     obj.ModTime(),
			Created:      obj.CreateTime(),
			HashInfoStr:  obj.GetHash().String(),
			HashInfo:     obj.GetHash().Export(),
			Sign:         common.Sign(obj, parentPath, isEncrypt(meta, reqPath)),
			Type:         utils.GetFileType(obj.GetName()),
			Thumb:        thumb,
			MountDetails: mountDetails,
			Extra:        extra,
		},
		RawURL:   rawURL,
		Readme:   getReadme(meta, reqPath),
		Header:   getHeader(meta, reqPath),
		Provider: provider,
		Related:  toObjsResp(related, parentPath, isEncrypt(parentMeta, parentPath)),
	})
}

type FsVideoPlayReq struct {
	Path     string `json:"path" form:"path"`
	Password string `json:"password" form:"password"`
}

// FsVideoPlay exposes the storage provider's official online-play (transcoded
// streaming) sources for a video at multiple resolutions, for drivers that
// implement driver.VideoPlayer (e.g. 115_open).
func FsVideoPlay(c *gin.Context) {
	var req FsVideoPlayReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	if user.IsGuest() && user.Disabled {
		common.ErrorStrResp(c, "Guest user is disabled, login please", 401)
		return
	}
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	meta, err := op.GetNearestMeta(reqPath)
	if err != nil && !errors.Is(errors.Cause(err), errs.MetaNotFound) {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.GinAppendValues(c, conf.MetaKey, meta)
	if !common.CanAccess(user, meta, reqPath, req.Password) {
		common.ErrorStrResp(c, "password is incorrect or you have no permission", 403)
		return
	}
	storage, err := fs.GetStorage(reqPath, &fs.GetStoragesArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	vp, ok := storage.(driver.VideoPlayer)
	if !ok {
		common.ErrorStrResp(c, "driver does not support official play sources", 400)
		return
	}
	obj, err := fs.Get(c.Request.Context(), reqPath, &fs.GetArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	sources, err := vp.VideoPlay(c.Request.Context(), obj)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	// Route every transcoded source through OpenList's signed video proxy so the
	// browser fetches them same-origin instead of hitting the provider CDN
	// directly (which fails CORS for 115's online-play HLS).
	apiURL := common.GetApiUrl(c)
	for i := range sources {
		if sources[i].URL != "" {
			sources[i].URL = BuildVideoProxyURL(apiURL, sources[i].URL)
		}
	}
	common.SuccessResp(c, sources)
}

// FsVideoSubtitle exposes the provider's subtitle tracks for a video, for
// drivers that implement driver.VideoSubtitleProvider (e.g. 115_open). These
// subtitles are independent of the play source, so the frontend can render them
// on every quality tier — including transcoded HLS streams that drop the
// original container's embedded subtitle tracks.
func FsVideoSubtitle(c *gin.Context) {
	var req FsVideoPlayReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	if user.IsGuest() && user.Disabled {
		common.ErrorStrResp(c, "Guest user is disabled, login please", 401)
		return
	}
	reqPath, err := user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	meta, err := op.GetNearestMeta(reqPath)
	if err != nil && !errors.Is(errors.Cause(err), errs.MetaNotFound) {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.GinAppendValues(c, conf.MetaKey, meta)
	if !common.CanAccess(user, meta, reqPath, req.Password) {
		common.ErrorStrResp(c, "password is incorrect or you have no permission", 403)
		return
	}
	storage, err := fs.GetStorage(reqPath, &fs.GetStoragesArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	obj, err := fs.Get(c.Request.Context(), reqPath, &fs.GetArgs{})
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	// Same-name sidecar subtitle files (any driver): list the folder once and
	// match basename siblings carrying a subtitle extension. This is the unified
	// subtitle source so the frontend doesn't have to compute it from `related`.
	subs := resolveSidecarSubtitles(c, reqPath, obj)
	// Provider subtitle tracks (e.g. 115 extracts a container's embedded subs
	// during transcoding) — appended when the driver supports them. Best-effort:
	// a provider error still returns the sidecar subs we already resolved.
	if vsp, ok := storage.(driver.VideoSubtitleProvider); ok {
		if providerSubs, perr := vsp.VideoSubtitle(c.Request.Context(), obj); perr == nil {
			// Route provider subtitle files through the signed video proxy so the
			// browser fetches them same-origin (provider CDNs reject cross-origin
			// subtitle fetches).
			apiURL := common.GetApiUrl(c)
			for i := range providerSubs {
				if providerSubs[i].URL != "" {
					providerSubs[i].URL = BuildVideoProxyURL(apiURL, providerSubs[i].URL)
				}
			}
			subs = append(subs, providerSubs...)
		}
	}
	common.SuccessResp(c, subs)
}

// subtitleExts are the sidecar subtitle file types matched as same-name siblings
// of a video. Matching is case-insensitive.
var subtitleExts = map[string]bool{
	"srt": true, "vtt": true, "ass": true, "ssa": true, "sup": true,
}

// matchSidecarSubtitle reports whether sibling is a same-name sidecar subtitle of
// the video (basename.<...>.<ext> with ext ∈ subtitleExts), returning the
// lowercased subtitle extension. It requires a '.' right after the video's
// basename so "Movie2.srt" doesn't match video "Movie.mkv", and ignores the file
// that is the video itself.
func matchSidecarSubtitle(videoName, siblingName string) (string, bool) {
	if strings.EqualFold(videoName, siblingName) {
		return "", false
	}
	ext := strings.TrimPrefix(strings.ToLower(stdpath.Ext(siblingName)), ".")
	if !subtitleExts[ext] {
		return "", false
	}
	base := strings.ToLower(strings.TrimSuffix(videoName, stdpath.Ext(videoName)))
	sib := strings.ToLower(siblingName)
	if !strings.HasPrefix(sib, base) || !strings.HasPrefix(sib[len(base):], ".") {
		return "", false
	}
	return ext, true
}

// resolveSidecarSubtitles lists the video's parent folder and returns the
// same-name sidecar subtitle files (multi-type / multi-language) as
// VideoSubtitleInfo with signed same-origin /p proxy URLs (mirroring the
// frontend proxyLink + FsGet rawURL construction).
func resolveSidecarSubtitles(c *gin.Context, reqPath string, video model.Obj) []driver.VideoSubtitleInfo {
	parentPath := stdpath.Dir(reqPath)
	siblings, err := fs.List(c.Request.Context(), parentPath, &fs.ListArgs{})
	if err != nil {
		return nil
	}
	parentMeta, _ := op.GetNearestMeta(parentPath)
	encrypt := isEncrypt(parentMeta, parentPath)
	apiURL := common.GetApiUrl(c)
	var subs []driver.VideoSubtitleInfo
	for _, o := range siblings {
		if o.IsDir() {
			continue
		}
		ext, ok := matchSidecarSubtitle(video.GetName(), o.GetName())
		if !ok {
			continue
		}
		query := ""
		if s := common.Sign(o, parentPath, encrypt); s != "" {
			query = "?sign=" + s
		}
		siblingPath := stdpath.Join(parentPath, o.GetName())
		subs = append(subs, driver.VideoSubtitleInfo{
			Title: strings.TrimSuffix(o.GetName(), stdpath.Ext(o.GetName())),
			Type:  ext,
			URL:   fmt.Sprintf("%s/p%s%s", apiURL, utils.EncodePath(siblingPath, true), query),
		})
	}
	return subs
}

func filterRelated(objs []model.Obj, obj model.Obj) []model.Obj {
	var related []model.Obj
	nameWithoutExt := strings.TrimSuffix(obj.GetName(), stdpath.Ext(obj.GetName()))
	for _, o := range objs {
		if o.GetName() == obj.GetName() {
			continue
		}
		if strings.HasPrefix(o.GetName(), nameWithoutExt) {
			related = append(related, o)
		}
	}
	return related
}

type FsOtherReq struct {
	model.FsOtherArgs
	Password string `json:"password" form:"password"`
}

func FsOther(c *gin.Context) {
	var req FsOtherReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	var err error
	req.Path, err = user.JoinPath(req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	meta, err := op.GetNearestMeta(req.Path)
	if err != nil && !errors.Is(errors.Cause(err), errs.MetaNotFound) {
		common.ErrorResp(c, err, 500)
		return
	}
	common.GinAppendValues(c, conf.MetaKey, meta)
	if !common.CanAccess(user, meta, req.Path, req.Password) {
		common.ErrorStrResp(c, "password is incorrect or you have no permission", 403)
		return
	}
	res, err := fs.Other(c.Request.Context(), req.FsOtherArgs)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	common.SuccessResp(c, res)
}
