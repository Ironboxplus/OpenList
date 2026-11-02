package handles

import (
	"fmt"
	"net/url"
	stdpath "path"
	"regexp"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/internal/sharing"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// shareLink is the one builder of share links: <apiURL>/<route>/<sid>/<path>,
// plus ?auth=<token> when the share has a password (?pwd= for a row that has no
// hash yet). route is "sd" (download) or "sad"
// (archive download). The password is query-escaped; the server reads it with
// c.Query("pwd"), which unescapes it, so any password round-trips.
func shareLink(apiURL, route string, s *model.SharingDB, sid, path string) string {
	u := fmt.Sprintf("%s/%s%s", apiURL, route, utils.EncodePath("/"+stdpath.Join(sid, path), true))
	if k, v := op.SharingLinkQuery(s); k != "" {
		u += "?" + k + "=" + url.QueryEscape(v)
	}
	return u
}

// previewParam marks a link generated for preview use (subtitles, thumbnails);
// it follows shareLink, which may or may not have put a query on the URL.
func previewParam(s *model.SharingDB) string {
	if s.HasPassword() {
		return "&preview=1"
	}
	return "?preview=1"
}

// shareToken is the access token a request presents: X-Share-Token, or ?auth=
// on direct links.
func shareToken(c *gin.Context) string {
	if t := c.GetHeader("X-Share-Token"); t != "" {
		return t
	}
	return c.Query("auth")
}

func SharingGet(c *gin.Context, req *FsGetReq) {
	sid, path, _ := strings.Cut(strings.TrimPrefix(req.Path, "/"), "/")
	if sid == "" {
		common.ErrorStrResp(c, "invalid share id", 400)
		return
	}
	s, obj, err := sharing.Get(c.Request.Context(), sid, path, model.SharingListArgs{
		Refresh:  false,
		Pwd:      req.Password,
		Token:    shareToken(c),
		ClientIP: c.ClientIP(),
	})
	if dealError(c, err) {
		return
	}
	url := ""
	if !obj.IsDir() {
		url = shareLink(common.GetApiUrl(c), "sd", s.SharingDB, sid, path)
	}
	thumb, _ := model.GetThumb(obj)
	common.SuccessResp(c, FsGetResp{
		ObjResp: ObjResp{
			Name:        obj.GetName(),
			Size:        obj.GetSize(),
			IsDir:       obj.IsDir(),
			Modified:    obj.ModTime(),
			Created:     obj.CreateTime(),
			HashInfoStr: obj.GetHash().String(),
			HashInfo:    obj.GetHash().Export(),
			Sign:        "",
			Type:        utils.GetObjType(obj.GetName(), obj.IsDir()),
			Thumb:       thumb,
		},
		RawURL:        url,
		Readme:        s.Readme,
		Header:        s.Header,
		Provider:      "unknown",
		Related:       nil,
		AllowPreview:  boolPtr(s.AllowPreview()),
		AllowDownload: boolPtr(s.AllowDownload()),
	})
}

// SharingVideoSubtitle is FsVideoSubtitle for a /@s/<sid>/<path> video: the
// sidecar subtitles next to it inside the share. Shares have no provider tracks.
// It lists through the sharing layer directly, so it does not count as a share
// access (SharingList/SharingGet already count the page view).
func SharingVideoSubtitle(c *gin.Context, req *FsVideoSubtitleReq) {
	sid, path, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(req.Path, "/@s"), "/"), "/")
	if sid == "" {
		common.ErrorStrResp(c, "invalid share id", 400)
		return
	}
	subs, err := sharedSidecarSubtitles(c, sid, path, req.Password)
	if dealError(c, err) {
		return
	}
	common.SuccessResp(c, subs)
}

func SharingList(c *gin.Context, req *ListReq) {
	sid, path, _ := strings.Cut(strings.TrimPrefix(req.Path, "/"), "/")
	if sid == "" {
		common.ErrorStrResp(c, "invalid share id", 400)
		return
	}
	s, objs, err := sharing.List(c.Request.Context(), sid, path, model.SharingListArgs{
		Refresh:  req.Refresh,
		Pwd:      req.Password,
		Token:    shareToken(c),
		ClientIP: c.ClientIP(),
	})
	if dealError(c, err) {
		return
	}
	total, objs := pagination(objs, &req.PageReq)
	common.SuccessResp(c, FsListResp{
		Content: utils.MustSliceConvert(objs, func(obj model.Obj) ObjResp {
			thumb, _ := model.GetThumb(obj)
			return ObjResp{
				Name:        obj.GetName(),
				Size:        obj.GetSize(),
				IsDir:       obj.IsDir(),
				Modified:    obj.ModTime(),
				Created:     obj.CreateTime(),
				HashInfoStr: obj.GetHash().String(),
				HashInfo:    obj.GetHash().Export(),
				Sign:        "",
				Thumb:       thumb,
				Type:        utils.GetObjType(obj.GetName(), obj.IsDir()),
			}
		}),
		Total:         int64(total),
		Readme:        s.Readme,
		Header:        s.Header,
		Write:         false,
		Provider:      "unknown",
		AllowPreview:  boolPtr(s.AllowPreview()),
		AllowDownload: boolPtr(s.AllowDownload()),
	})
}

func SharingArchiveMeta(c *gin.Context, req *ArchiveMetaReq) {
	if !setting.GetBool(conf.ShareArchivePreview) {
		common.ErrorStrResp(c, "sharing archives previewing is not allowed", 403)
		return
	}
	sid, path, _ := strings.Cut(strings.TrimPrefix(req.Path, "/"), "/")
	if sid == "" {
		common.ErrorStrResp(c, "invalid share id", 400)
		return
	}
	archiveArgs := model.ArchiveArgs{
		LinkArgs: model.LinkArgs{
			Header: c.Request.Header,
			Type:   c.Query("type"),
		},
		Password: req.ArchivePass,
	}
	s, ret, err := sharing.ArchiveMeta(c.Request.Context(), sid, path, model.SharingArchiveMetaArgs{
		ArchiveMetaArgs: model.ArchiveMetaArgs{
			ArchiveArgs: archiveArgs,
			Refresh:     req.Refresh,
		},
		Pwd:      req.Password,
		Token:    shareToken(c),
		ClientIP: c.ClientIP(),
	})
	if dealError(c, err) {
		return
	}
	url := shareLink(common.GetApiUrl(c), "sad", s.SharingDB, sid, path)
	common.SuccessResp(c, ArchiveMetaResp{
		Comment:     ret.GetComment(),
		IsEncrypted: ret.IsEncrypted(),
		Content:     toContentResp(ret.GetTree()),
		Sort:        ret.Sort,
		RawURL:      url,
		Sign:        "",
	})
}

func SharingArchiveList(c *gin.Context, req *ArchiveListReq) {
	if !setting.GetBool(conf.ShareArchivePreview) {
		common.ErrorStrResp(c, "sharing archives previewing is not allowed", 403)
		return
	}
	sid, path, _ := strings.Cut(strings.TrimPrefix(req.Path, "/"), "/")
	if sid == "" {
		common.ErrorStrResp(c, "invalid share id", 400)
		return
	}
	innerArgs := model.ArchiveInnerArgs{
		ArchiveArgs: model.ArchiveArgs{
			LinkArgs: model.LinkArgs{
				Header: c.Request.Header,
				Type:   c.Query("type"),
			},
			Password: req.ArchivePass,
		},
		InnerPath: utils.FixAndCleanPath(req.InnerPath),
	}
	_, objs, err := sharing.ArchiveList(c.Request.Context(), sid, path, model.SharingArchiveListArgs{
		ArchiveListArgs: model.ArchiveListArgs{
			ArchiveInnerArgs: innerArgs,
			Refresh:          req.Refresh,
		},
		Pwd:      req.Password,
		Token:    shareToken(c),
		ClientIP: c.ClientIP(),
	})
	if dealError(c, err) {
		return
	}
	total, objs := pagination(objs, &req.PageReq)
	ret, _ := utils.SliceConvert(objs, func(src model.Obj) (ObjResp, error) {
		return toObjsRespWithoutSignAndThumb(src), nil
	})
	common.SuccessResp(c, common.PageResp{
		Content: ret,
		Total:   int64(total),
	})
}

func SharingDown(c *gin.Context) {
	sid := c.Request.Context().Value(conf.SharingIDKey).(string)
	path := c.Request.Context().Value(conf.PathKey).(string)
	path = utils.FixAndCleanPath(path)
	access := shareAccess(c)
	s, err := op.GetSharingById(sid)
	if err != nil {
		err = errors.WithStack(errs.SharingNotFound)
	} else {
		if len(s.Files) != 1 && path == "/" {
			err = errors.New("cannot get sharing root link")
		} else {
			err = op.CheckSharingContent(access, s, true)
		}
	}
	if dealErrorPage(c, err) {
		return
	}
	unwrapPath, err := op.GetSharingUnwrapPath(s, path)
	if err != nil {
		common.ErrorPage(c, errors.New("failed get sharing unwrap path"), 500)
		return
	}
	storage, actualPath, err := op.GetStorageAndActualPath(unwrapPath)
	if err != nil {
		log.Errorf("failed get sharing storage: %+v", err)
		common.ErrorPage(c, errors.New("failed get sharing link"), 500)
		return
	}
	if setting.GetBool(conf.ShareForceProxy) || common.ShouldProxy(storage, stdpath.Base(actualPath)) {
		if _, ok := c.GetQuery("d"); !ok {
			if url := common.GenerateDownProxyURL(storage.GetStorage(), unwrapPath); url != "" {
				c.Redirect(302, url)
				return
			}
		}
		link, obj, err := op.Link(c.Request.Context(), storage, actualPath, model.LinkArgs{
			Header: c.Request.Header,
			Type:   c.Query("type"),
		})
		if err != nil {
			log.Errorf("failed get sharing link: %+v", err)
			common.ErrorPage(c, errors.New("failed get sharing link"), 500)
			return
		}
		if dealErrorPage(c, op.ChargeSharing(access, s)) {
			link.Close()
			return
		}
		proxy(c, link, obj, storage.GetStorage().ProxyRange)
	} else {
		link, _, err := op.Link(c.Request.Context(), storage, actualPath, model.LinkArgs{
			IP:       c.ClientIP(),
			Header:   c.Request.Header,
			Type:     c.Query("type"),
			Redirect: true,
		})
		if err != nil {
			log.Errorf("failed get sharing link: %+v", err)
			common.ErrorPage(c, errors.New("failed get sharing link"), 500)
			return
		}
		if dealErrorPage(c, op.ChargeSharing(access, s)) {
			link.Close()
			return
		}
		redirect(c, link)
	}
}

func SharingArchiveExtract(c *gin.Context) {
	if !setting.GetBool(conf.ShareArchivePreview) {
		common.ErrorPage(c, errors.New("sharing archives previewing is not allowed"), 403)
		return
	}
	sid := c.Request.Context().Value(conf.SharingIDKey).(string)
	path := c.Request.Context().Value(conf.PathKey).(string)
	path = utils.FixAndCleanPath(path)
	access := shareAccess(c)
	innerPath := utils.FixAndCleanPath(c.Query("inner"))
	archivePass := c.Query("pass")
	s, err := op.GetSharingById(sid)
	if err != nil {
		err = errors.WithStack(errs.SharingNotFound)
	} else {
		if len(s.Files) != 1 && path == "/" {
			err = errors.New("cannot extract sharing root")
		} else {
			err = op.CheckSharingContent(access, s, true)
		}
	}
	if dealErrorPage(c, err) {
		return
	}
	unwrapPath, err := op.GetSharingUnwrapPath(s, path)
	if err != nil {
		common.ErrorPage(c, errors.New("failed get sharing unwrap path"), 500)
		return
	}
	storage, actualPath, err := op.GetStorageAndActualPath(unwrapPath)
	if dealErrorPage(c, err) {
		return
	}
	args := model.ArchiveInnerArgs{
		ArchiveArgs: model.ArchiveArgs{
			LinkArgs: model.LinkArgs{
				Header: c.Request.Header,
				Type:   c.Query("type"),
			},
			Password: archivePass,
		},
		InnerPath: innerPath,
	}
	if _, ok := storage.(driver.ArchiveReader); ok {
		if setting.GetBool(conf.ShareForceProxy) || common.ShouldProxy(storage, stdpath.Base(actualPath)) {
			link, obj, err := op.DriverExtract(c.Request.Context(), storage, actualPath, args)
			if dealErrorPage(c, err) {
				return
			}
			if dealErrorPage(c, op.ChargeSharing(access, s)) {
				link.Close()
				return
			}
			proxy(c, link, obj, storage.GetStorage().ProxyRange)
		} else {
			args.Redirect = true
			link, _, err := op.DriverExtract(c.Request.Context(), storage, actualPath, args)
			if dealErrorPage(c, err) {
				return
			}
			if dealErrorPage(c, op.ChargeSharing(access, s)) {
				link.Close()
				return
			}
			redirect(c, link)
		}
	} else {
		rc, size, err := op.InternalExtract(c.Request.Context(), storage, actualPath, args)
		if dealErrorPage(c, err) {
			return
		}
		if dealErrorPage(c, op.ChargeSharing(access, s)) {
			_ = rc.Close()
			return
		}
		fileName := stdpath.Base(innerPath)
		proxyInternalExtract(c, rc, size, fileName)
	}
}

// Response codes of a share request. 403 means "password required or wrong"
// and nothing else, so a client may open its password prompt on it.
const (
	shareCodeNotFound   = 404 // no such share
	shareCodeGone       = 410 // expired, disabled, or its access limit is used up
	shareCodeNeedPwd    = 403 // password missing or wrong
	shareCodeTooMany    = 429 // too many wrong passwords from this IP, wait
	shareCodeNotAllowed = 423 // the share does not allow this (preview or download)
)

func dealError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	} else if errors.Is(err, errs.SharingNotFound) {
		common.ErrorStrResp(c, "the share does not exist", shareCodeNotFound)
	} else if errors.Is(err, errs.InvalidSharing) {
		common.ErrorStrResp(c, "the share has expired or is no longer valid", shareCodeGone)
	} else if errors.Is(err, errs.ShareActionNotAllowed) {
		common.ErrorResp(c, err, shareCodeNotAllowed)
	} else if errors.Is(err, errs.WrongShareCode) {
		common.ErrorResp(c, err, shareCodeNeedPwd)
	} else if errors.Is(err, errs.TooManyShareAttempts) {
		common.ErrorResp(c, err, shareCodeTooMany)
	} else if errors.Is(err, errs.WrongArchivePassword) {
		common.ErrorResp(c, err, 202)
	} else {
		common.ErrorResp(c, err, 500)
	}
	return true
}

func dealErrorPage(c *gin.Context, err error) bool {
	if err == nil {
		return false
	} else if errors.Is(err, errs.SharingNotFound) {
		common.ErrorPage(c, errors.New("the share does not exist"), shareCodeNotFound)
	} else if errors.Is(err, errs.InvalidSharing) {
		common.ErrorPage(c, errors.New("the share has expired or is no longer valid"), shareCodeGone)
	} else if errors.Is(err, errs.ShareActionNotAllowed) {
		common.ErrorPage(c, err, shareCodeNotAllowed)
	} else if errors.Is(err, errs.WrongShareCode) {
		common.ErrorPage(c, err, shareCodeNeedPwd)
	} else if errors.Is(err, errs.TooManyShareAttempts) {
		common.ErrorPage(c, err, shareCodeTooMany)
	} else if errors.Is(err, errs.WrongArchivePassword) {
		common.ErrorPage(c, err, 202)
	} else {
		common.ErrorPage(c, err, 500)
	}
	return true
}

type SharingResp struct {
	*model.Sharing
	CreatorName   string `json:"creator"`
	CreatorRole   int    `json:"creator_role"`
	HasPassword   bool   `json:"has_password"`
	AllowPreview  bool   `json:"allow_preview"`
	AllowDownload bool   `json:"allow_download"`
}

func sharingResp(s *model.Sharing) SharingResp {
	return SharingResp{
		Sharing:       s,
		CreatorName:   s.Creator.Username,
		CreatorRole:   s.Creator.Role,
		HasPassword:   s.HasPassword(),
		AllowPreview:  s.AllowPreview(),
		AllowDownload: s.AllowDownload(),
	}
}

// shareAccess reads what a /sd or /sad request presents: the old ?pwd=, the
// ?auth= token, whether it is a preview (preview=1, or a thumbnail), the HEAD
// method, and the start of a Range. preview=1 is a policy marker for the UI and
// honest clients, not DRM: the same bytes are served either way.
func shareAccess(c *gin.Context) op.SharingAccess {
	return op.SharingAccess{
		IP:         c.ClientIP(),
		Pwd:        c.Query("pwd"),
		Token:      shareToken(c),
		Preview:    c.Query("preview") == "1" || c.Query("type") == "thumb",
		Head:       c.Request.Method == "HEAD",
		RangeStart: op.RangeStart(c.GetHeader("Range")),
	}
}

type sharingAuthReq struct {
	ID       string `json:"id"`
	Password string `json:"password"`
}

// SharingAuth trades a share password for an access token (POST body, so the
// password never sits in a URL). The token is what links carry as ?auth=.
func SharingAuth(c *gin.Context) {
	var req sharingAuthReq
	if err := c.ShouldBindJSON(&req); err != nil || req.ID == "" {
		common.ErrorStrResp(c, "invalid request", 400)
		return
	}
	s, err := op.GetSharingById(req.ID)
	if err != nil {
		dealError(c, errors.WithStack(errs.SharingNotFound))
		return
	}
	token, err := op.AuthSharing(c.ClientIP(), req.Password, s)
	if dealError(c, err) {
		return
	}
	common.SuccessResp(c, gin.H{"token": token})
}

func GetSharing(c *gin.Context) {
	sid := c.Query("id")
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	s, err := op.GetSharingById(sid)
	if err != nil || (!user.IsAdmin() && s.Creator.ID != user.ID) {
		common.ErrorStrResp(c, "sharing not found", 404)
		return
	}
	common.SuccessResp(c, sharingResp(s))
}

func ListSharings(c *gin.Context) {
	var req model.PageReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	req.Validate()
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	var sharings []model.Sharing
	var total int64
	var err error
	if user.IsAdmin() {
		sharings, total, err = op.GetSharings(req.Page, req.PerPage)
	} else {
		sharings, total, err = op.GetSharingsByCreatorId(user.ID, req.Page, req.PerPage)
	}
	if err != nil {
		common.ErrorResp(c, err, 500, true)
		return
	}
	common.SuccessResp(c, common.PageResp{
		Content: utils.MustSliceConvert(sharings, func(s model.Sharing) SharingResp {
			return sharingResp(&s)
		}),
		Total: total,
	})
}

type UpdateSharingReq struct {
	Files       []string   `json:"files"`
	Expires     *time.Time `json:"expires"`
	Pwd         string     `json:"pwd"`
	MaxAccessed int        `json:"max_accessed"`
	// BurnAfterRead, AllowPreview and AllowDownload are pointers so that a client
	// that does not know them (an older UI) leaves them as they are.
	BurnAfterRead *bool  `json:"burn_after_read"`
	AllowPreview  *bool  `json:"allow_preview"`
	AllowDownload *bool  `json:"allow_download"`
	Disabled      bool   `json:"disabled"`
	Remark        string `json:"remark"`
	Readme        string `json:"readme"`
	Header        string `json:"header"`
	model.Sort
	CreatorName string `json:"creator"`
	Accessed    int    `json:"accessed"`
	ID          string `json:"id"`
	NewID       string `json:"new_id"`
}

var validSharingID = regexp.MustCompile(`^[\w\p{Han}\-]+$`)

func validateSharingID(id string) error {
	if len([]rune(id)) > 64 {
		return errors.New("share id must be at most 64 characters")
	}
	if !validSharingID.MatchString(id) {
		return errors.New("share id can only contain letters, numbers, underscores, hyphens, and CJK characters")
	}
	return nil
}

func applyShareFlags(s *model.SharingDB, req *UpdateSharingReq) {
	if req.BurnAfterRead != nil {
		s.BurnAfterRead = *req.BurnAfterRead
	}
	if req.AllowPreview != nil {
		s.DenyPreview = !*req.AllowPreview
	}
	if req.AllowDownload != nil {
		s.DenyDownload = !*req.AllowDownload
	}
}

func UpdateSharing(c *gin.Context) {
	var req UpdateSharingReq
	if err := c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if len(req.Files) == 0 || (len(req.Files) == 1 && req.Files[0] == "") {
		common.ErrorStrResp(c, "must add at least 1 object", 400)
		return
	}
	var user *model.User
	var err error
	reqUser := c.Request.Context().Value(conf.UserKey).(*model.User)
	if reqUser.IsAdmin() && req.CreatorName != "" {
		user, err = op.GetUserByName(req.CreatorName)
		if err != nil {
			common.ErrorStrResp(c, "no such a user", 400)
			return
		}
	} else {
		user = reqUser
		if !user.CanShare() {
			common.ErrorStrResp(c, "permission denied", 403)
			return
		}
	}
	for i, s := range req.Files {
		s = utils.FixAndCleanPath(s)
		req.Files[i] = s
		if !reqUser.IsAdmin() && !utils.IsSubPath(user.BasePath, s) {
			common.ErrorStrResp(c, fmt.Sprintf("permission denied to share path [%s]", s), 500)
			return
		}
	}
	s, err := op.GetSharingById(req.ID)
	if err != nil || (!reqUser.IsAdmin() && s.CreatorId != user.ID) {
		common.ErrorStrResp(c, "sharing not found", 404)
		return
	}
	if reqUser.IsAdmin() && req.CreatorName == "" {
		user = s.Creator
	}
	s.Files = req.Files
	s.Expires = req.Expires
	if req.Pwd != s.Pwd || (req.Pwd != "" && s.PasswordHash == "") {
		s.SetPassword(req.Pwd) // a new password also invalidates issued tokens
	}
	applyShareFlags(s.SharingDB, &req)
	s.Accessed = req.Accessed
	s.MaxAccessed = req.MaxAccessed
	s.Disabled = req.Disabled
	s.Sort = req.Sort
	s.Header = req.Header
	s.Readme = req.Readme
	s.Remark = req.Remark
	s.Creator = user
	if req.NewID != "" && req.NewID != req.ID {
		if !reqUser.CanCustomizeShareID() {
			common.ErrorStrResp(c, "permission denied", 403)
			return
		}
		if err = validateSharingID(req.NewID); err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
		if err = op.UpdateSharingId(s, req.NewID); err != nil {
			common.ErrorResp(c, err, 500)
			return
		}
	}
	if !s.LimitReached() {
		s.ConsumedAt = nil // the limit was raised or lifted: the share is open again
	}
	if err = op.UpdateSharing(s); err != nil {
		common.ErrorResp(c, err, 500)
	} else {
		common.SuccessResp(c, sharingResp(s))
	}
}

func CreateSharing(c *gin.Context) {
	var req UpdateSharingReq
	var err error
	if err = c.ShouldBind(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if len(req.Files) == 0 || (len(req.Files) == 1 && req.Files[0] == "") {
		common.ErrorStrResp(c, "must add at least 1 object", 400)
		return
	}
	if req.ID != "" {
		if err = validateSharingID(req.ID); err != nil {
			common.ErrorResp(c, err, 400)
			return
		}
	}
	var user *model.User
	reqUser := c.Request.Context().Value(conf.UserKey).(*model.User)
	if reqUser.IsAdmin() && req.CreatorName != "" {
		user, err = op.GetUserByName(req.CreatorName)
		if err != nil {
			common.ErrorStrResp(c, "no such a user", 400)
			return
		}
	} else {
		user = reqUser
		if !user.CanShare() || (!user.CanCustomizeShareID() && req.ID != "") {
			common.ErrorStrResp(c, "permission denied", 403)
			return
		}
	}
	for i, s := range req.Files {
		s = utils.FixAndCleanPath(s)
		req.Files[i] = s
		if !reqUser.IsAdmin() && !utils.IsSubPath(user.BasePath, s) {
			common.ErrorStrResp(c, fmt.Sprintf("permission denied to share path [%s]", s), 500)
			return
		}
	}
	s := &model.Sharing{
		SharingDB: &model.SharingDB{
			ID:          req.ID,
			Expires:     req.Expires,
			Accessed:    req.Accessed,
			MaxAccessed: req.MaxAccessed,
			Disabled:    req.Disabled,
			Sort:        req.Sort,
			Remark:      req.Remark,
			Readme:      req.Readme,
			Header:      req.Header,
		},
		Files:   req.Files,
		Creator: user,
	}
	s.SetPassword(req.Pwd)
	applyShareFlags(s.SharingDB, &req)
	var id string
	if id, err = op.CreateSharing(s); err != nil {
		common.ErrorResp(c, err, 500)
	} else {
		s.ID = id
		common.SuccessResp(c, sharingResp(s))
	}
}

func DeleteSharing(c *gin.Context) {
	sid := c.Query("id")
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	s, err := op.GetSharingById(sid)
	if err != nil || (!user.IsAdmin() && s.CreatorId != user.ID) {
		common.ErrorResp(c, err, 404)
		return
	}
	if err = op.DeleteSharing(sid); err != nil {
		common.ErrorResp(c, err, 500)
	} else {
		common.SuccessResp(c)
	}
}

func SetEnableSharing(disable bool) func(ctx *gin.Context) {
	return func(c *gin.Context) {
		sid := c.Query("id")
		user := c.Request.Context().Value(conf.UserKey).(*model.User)
		s, err := op.GetSharingById(sid)
		if err != nil || (!user.IsAdmin() && s.CreatorId != user.ID) {
			common.ErrorStrResp(c, "sharing not found", 404)
			return
		}
		s.Disabled = disable
		if err = op.UpdateSharing(s, true); err != nil {
			common.ErrorResp(c, err, 500)
		} else {
			common.SuccessResp(c)
		}
	}
}

func boolPtr(b bool) *bool { return &b }
