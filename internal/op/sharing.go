package op

import (
	"fmt"
	stdpath "path"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/sign"
	"github.com/OpenListTeam/OpenList/v4/pkg/singleflight"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/go-cache"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

func makeJoined(sdb []model.SharingDB) []model.Sharing {
	creator := make(map[uint]*model.User)
	return utils.MustSliceConvert(sdb, func(s model.SharingDB) model.Sharing {
		var c *model.User
		var ok bool
		if c, ok = creator[s.CreatorId]; !ok {
			var err error
			if c, err = GetUserById(s.CreatorId); err != nil {
				c = nil
			} else {
				creator[s.CreatorId] = c
			}
		}
		var files []string
		if err := utils.Json.UnmarshalFromString(s.FilesRaw, &files); err != nil {
			files = make([]string, 0)
		}
		return model.Sharing{
			SharingDB: &s,
			Files:     files,
			Creator:   c,
		}
	})
}

var sharingCache = cache.NewMemCache(cache.WithShards[*model.Sharing](8))
var sharingG singleflight.Group[*model.Sharing]
var sharingAccessCache = cache.NewMemCache[struct{}]()

const sharingAccessCountDelay = 30 * time.Minute

func sharingAccessKey(id, ip string) string {
	return id + ":" + ip
}

var sharingAccessG singleflight.Group[struct{}]

// SharingAccess describes one request for the content of a share.
type SharingAccess struct {
	IP    string
	Pwd   string // password presented (the ?pwd= of an old link, or the API field)
	Token string // access token presented (?auth= / X-Share-Token)
	// Preview is the client saying "this is a preview, not a download" (preview=1
	// on the link). A policy switch for the UI and honest clients, not DRM: the
	// bytes served are the same.
	Preview bool
	Head    bool
	// RangeStart is the first byte of a Range request, -1 when there is none.
	RangeStart int64
}

func plainAccess(ip, pwd string) SharingAccess {
	return SharingAccess{IP: ip, Pwd: pwd, RangeStart: -1}
}

// Failed share passwords are throttled per (IP, share) with the numbers of the
// web login (model.DefaultMaxAuthRetries / DefaultLockDuration). AList has no
// limit on share passwords; OWASP's Authentication Cheat Sheet recommends
// throttling failed attempts. The settings share_pwd_max_attempts and
// share_pwd_lock_seconds override these defaults, which are ours.
var sharingFailCache = cache.NewMemCache[int]()

func sharingFailLimits() (int, time.Duration) {
	max, lock := model.DefaultMaxAuthRetries, model.DefaultLockDuration
	if it, err := GetSettingItemByKey(conf.SharePwdMaxAttempts); err == nil {
		if v, err := strconv.Atoi(it.Value); err == nil && v > 0 {
			max = v
		}
	}
	if it, err := GetSettingItemByKey(conf.SharePwdLockSeconds); err == nil {
		if v, err := strconv.Atoi(it.Value); err == nil && v > 0 {
			lock = time.Duration(v) * time.Second
		}
	}
	return max, lock
}

func validateSharing(a SharingAccess, s *model.Sharing, content bool) error {
	if !s.Usable() {
		return errors.WithStack(errs.InvalidSharing)
	}
	if s.HasPassword() {
		failKey := sharingAccessKey(s.ID, a.IP)
		presented := a.Pwd != "" || a.Token != ""
		if presented {
			max, _ := sharingFailLimits()
			if n, _ := sharingFailCache.Get(failKey); n >= max {
				return errors.WithStack(errs.TooManyShareAttempts)
			}
		}
		switch {
		case a.Token != "" && VerifySharingToken(s.SharingDB, a.Token) == nil:
			// a valid token stands for the password
		case s.Verify(a.Pwd):
			if presented {
				sharingFailCache.Del(failKey)
			}
		default:
			if presented {
				_, lock := sharingFailLimits()
				n, _ := sharingFailCache.Get(failKey)
				sharingFailCache.Set(failKey, n+1, cache.WithEx[int](lock))
			}
			return errors.WithStack(errs.WrongShareCode)
		}
	}
	if content {
		if a.Preview && s.DenyPreview || !a.Preview && s.DenyDownload {
			return errors.WithStack(errs.ShareActionNotAllowed)
		}
	}
	return nil
}

// CheckSharing is the read-only gate used for browsing: the share is usable,
// the password is right, and the limit is not reached (unless this visitor
// already holds a slot). It counts nothing.
func CheckSharing(ip, pwd string, s *model.Sharing) error {
	return CheckSharingContent(plainAccess(ip, pwd), s, false)
}

// CheckSharingContent is CheckSharing for a request for file content (content
// true also applies the preview/download switches). HEAD requests stop here:
// they never take a slot.
func CheckSharingContent(a SharingAccess, s *model.Sharing, content bool) error {
	if err := validateSharing(a, s, content); err != nil {
		return err
	}
	key := sharingAccessKey(s.ID, a.IP)
	if s.LimitReached() && !sharingAccessCache.Exists(key) {
		_, err, _ := sharingAccessG.Do(key, func() (struct{}, error) {
			return struct{}{}, nil
		})
		if err != nil {
			return err
		}
		if !sharingAccessCache.Exists(key) {
			return errors.WithStack(errs.InvalidSharing)
		}
	}
	return nil
}

// AccessSharing takes the download slot for a request without range or preview
// information; see ChargeSharing.
func AccessSharing(ip, pwd string, s *model.Sharing) error {
	return ChargeSharing(plainAccess(ip, pwd), s)
}

// ChargeSharing is the one place where a request for share content is counted.
// Call it after the link was resolved, so a failed request costs nothing.
//
//   - HEAD: validated, never counted.
//   - the slot (Accessed / MaxAccessed / burn after read / ConsumedAt) is taken
//     once per (share, IP) per 30 minutes by the first request of any kind
//     (download or preview, ranged or not); that window is what lets a player's
//     later range requests keep working after the last slot is taken.
//   - DownloadCount grows for each download that starts a file: not a preview,
//     and no Range header or one starting at byte 0.
func ChargeSharing(a SharingAccess, s *model.Sharing) error {
	if err := validateSharing(a, s, true); err != nil {
		return err
	}
	if a.Head {
		return nil
	}
	key := sharingAccessKey(s.ID, a.IP)
	_, err, _ := sharingAccessG.Do(key, func() (struct{}, error) {
		if sharingAccessCache.Exists(key) {
			return struct{}{}, nil
		}
		ok, err := db.TakeSharingSlot(s.ID)
		if err != nil {
			return struct{}{}, err
		}
		if !ok {
			return struct{}{}, errors.WithStack(errs.InvalidSharing)
		}
		sharingCache.Del(s.ID)
		sharingAccessCache.Set(key, struct{}{}, cache.WithEx[struct{}](sharingAccessCountDelay))
		return struct{}{}, nil
	})
	if err != nil {
		return err
	}
	if !a.Preview && (a.RangeStart == -1 || a.RangeStart == 0) {
		if err := db.AddSharingStats(s.ID, 0, 1); err != nil {
			log.Warnf("count sharing download: %v", err)
		}
	}
	return nil
}

// CountSharingView counts a visit to the share's root: once per (share, IP) per
// 30 minutes, not per directory click or revalidation.
func CountSharingView(ip string, s *model.Sharing) {
	key := "v:" + sharingAccessKey(s.ID, ip)
	if sharingAccessCache.Exists(key) {
		return
	}
	sharingAccessCache.Set(key, struct{}{}, cache.WithEx[struct{}](sharingAccessCountDelay))
	if err := db.AddSharingStats(s.ID, 1, 0); err != nil {
		log.Warnf("count sharing view: %v", err)
	}
}

const sharingTokenLifetime = 24 * time.Hour

func sharingSigner() sign.Sign {
	secret := ""
	if it, err := GetSettingItemByKey(conf.Token); err == nil {
		secret = it.Value
	}
	return sign.NewHMACSign([]byte(secret + "-sharing-access"))
}

func sharingTokenPayload(s *model.SharingDB) string { return s.ID + ":" + s.PasswordHash }

// SignSharingToken is the access token a visitor gets for a share after giving
// the password once: valid for 24 hours or until the share expires, and for as
// long as the password stays the same. It goes in links as ?auth=, so the
// password itself never has to.
func SignSharingToken(s *model.SharingDB) string {
	ttl := sharingTokenLifetime
	if s.Expires != nil && !s.Expires.IsZero() {
		if left := time.Until(*s.Expires); left < ttl {
			ttl = left
		}
	}
	return sharingSigner().Sign(sharingTokenPayload(s), time.Now().Add(ttl).Unix())
}

func VerifySharingToken(s *model.SharingDB, token string) error {
	if s.PasswordHash == "" {
		return errors.WithStack(errs.WrongShareCode)
	}
	return sharingSigner().Verify(sharingTokenPayload(s), token)
}

// AuthSharing checks a password and, if right, returns an access token ("" for
// a share without password).
func AuthSharing(ip, pwd string, s *model.Sharing) (string, error) {
	if err := validateSharing(plainAccess(ip, pwd), s, false); err != nil {
		return "", err
	}
	if !s.HasPassword() {
		return "", nil
	}
	return SignSharingToken(s.SharingDB), nil
}

func GetSharingById(id string, refresh ...bool) (*model.Sharing, error) {
	if !utils.IsBool(refresh...) {
		if sharing, ok := sharingCache.Get(id); ok {
			log.Debugf("use cache when get sharing %s", id)
			return sharing, nil
		}
	}
	sharing, err, _ := sharingG.Do(id, func() (*model.Sharing, error) {
		s, err := db.GetSharingById(id)
		if err != nil {
			return nil, errors.WithMessagef(err, "failed get sharing [%s]", id)
		}
		creator, err := GetUserById(s.CreatorId)
		if err != nil {
			return nil, errors.WithMessagef(err, "failed get sharing creator [%s]", id)
		}
		var files []string
		if err = utils.Json.UnmarshalFromString(s.FilesRaw, &files); err != nil {
			files = make([]string, 0)
		}
		return &model.Sharing{
			SharingDB: s,
			Files:     files,
			Creator:   creator,
		}, nil
	})
	return sharing, err
}

func GetSharings(pageIndex, pageSize int) ([]model.Sharing, int64, error) {
	s, cnt, err := db.GetSharings(pageIndex, pageSize)
	if err != nil {
		return nil, 0, errors.WithStack(err)
	}
	return makeJoined(s), cnt, nil
}

func GetSharingsByCreatorId(userId uint, pageIndex, pageSize int) ([]model.Sharing, int64, error) {
	s, cnt, err := db.GetSharingsByCreatorId(userId, pageIndex, pageSize)
	if err != nil {
		return nil, 0, errors.WithStack(err)
	}
	return makeJoined(s), cnt, nil
}

func GetSharingUnwrapPath(sharing *model.Sharing, path string) (unwrapPath string, err error) {
	if len(sharing.Files) == 0 {
		return "", errors.New("cannot get actual path of an invalid sharing")
	}
	// Re-validate that the shared paths are still within the creator's current
	// BasePath. This prevents access to files that fell out-of-scope after the
	// creator's BasePath was changed by an admin.
	if sharing.Creator != nil && !sharing.Creator.IsAdmin() {
		for _, f := range sharing.Files {
			if !utils.IsSubPath(sharing.Creator.BasePath, f) {
				return "", errors.Errorf("sharing path [%s] is outside the creator's base path", f)
			}
		}
	}
	if len(sharing.Files) == 1 {
		return stdpath.Join(sharing.Files[0], path), nil
	}
	path = utils.FixAndCleanPath(path)[1:]
	if len(path) == 0 {
		return "", errors.New("cannot get actual path of a sharing root path")
	}
	mapPath := ""
	child, rest, _ := strings.Cut(path, "/")
	for _, c := range sharing.Files {
		if child == stdpath.Base(c) {
			mapPath = c
			break
		}
	}
	if mapPath == "" {
		return "", fmt.Errorf("failed find child [%s] of sharing [%s]", child, sharing.ID)
	}
	return stdpath.Join(mapPath, rest), nil
}

func CreateSharing(sharing *model.Sharing) (id string, err error) {
	sharing.CreatorId = sharing.Creator.ID
	sharing.FilesRaw, err = utils.Json.MarshalToString(utils.MustSliceConvert(sharing.Files, utils.FixAndCleanPath))
	if err != nil {
		return "", errors.WithStack(err)
	}
	return db.CreateSharing(sharing.SharingDB)
}

func UpdateSharing(sharing *model.Sharing, skipMarshal ...bool) (err error) {
	if !utils.IsBool(skipMarshal...) {
		sharing.CreatorId = sharing.Creator.ID
		sharing.FilesRaw, err = utils.Json.MarshalToString(utils.MustSliceConvert(sharing.Files, utils.FixAndCleanPath))
		if err != nil {
			return errors.WithStack(err)
		}
	}
	sharingCache.Del(sharing.ID)
	return db.UpdateSharing(sharing.SharingDB)
}

func UpdateSharingId(sharing *model.Sharing, newId string) error {
	sharingCache.Del(sharing.ID)
	if err := db.UpdateSharingId(sharing.ID, newId); err != nil {
		return err
	}
	sharing.ID = newId
	return nil
}

func DeleteSharing(sid string) error {
	sharingCache.Del(sid)
	return db.DeleteSharingById(sid)
}

func DeleteSharingsByCreatorId(creatorId uint) error {
	return db.DeleteSharingsByCreatorId(creatorId)
}

// RangeStart is the first byte of a "bytes=N-M" Range header: -1 when there is
// no Range header, -2 when there is one that does not start at a known byte
// (a suffix range, or garbage).
func RangeStart(h string) int64 {
	rest, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !ok {
		return -1
	}
	first, _, _ := strings.Cut(rest, ",")
	start, _, ok := strings.Cut(strings.TrimSpace(first), "-")
	if !ok || start == "" {
		return -2
	}
	n, err := strconv.ParseInt(start, 10, 64)
	if err != nil || n < 0 {
		return -2
	}
	return n
}

// SharingLinkQuery is the credential a generated share link carries: the access
// token ("auth") for a share with a password hash, the old "pwd" for a row that
// has none yet, nothing for an open share.
func SharingLinkQuery(s *model.SharingDB) (key, val string) {
	switch {
	case s.PasswordHash != "":
		return "auth", SignSharingToken(s)
	case s.Pwd != "":
		return "pwd", s.Pwd
	}
	return "", ""
}
