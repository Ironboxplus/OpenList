package quark

import (
	"context"
	"encoding/hex"
	"fmt"
	"hash"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	streamPkg "github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/avast/retry-go"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

const (
	mkdirConflictMessage          = "file is doloading[同名冲突]"
	mkdirVisibilityAttempts       = 6
	mkdirVisibilityInitialBackoff = 200 * time.Millisecond
	mkdirVisibilityMaxBackoff     = 800 * time.Millisecond
	mkdirVisibilityMaxPages       = 1000
)

type QuarkOrUC struct {
	model.Storage
	Addition
	config driver.Config
	conf   Conf

	// client 用于测试时注入自定义 client，nil 时使用全局 base.RestyClient
	client *resty.Client

	// cookieMu 保护 d.Cookie 的读-改-写，避免多 goroutine（定时刷新 + 并发业务请求）竞态
	cookieMu sync.Mutex

	refreshMu sync.Mutex
	cancel    context.CancelFunc
}

func (d *QuarkOrUC) Config() driver.Config {
	return d.config
}

func (d *QuarkOrUC) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *QuarkOrUC) Init(ctx context.Context) error {
	if d.config.Name == "UC" {
		if d.UTDID == "" {
			d.UTDID = newUCDeviceID()
			op.MustSaveDriverStorage(d)
		}
		if _, err := ucDownloadToken(d.UTDID); err != nil {
			return err
		}
	}
	_, err := d.request("/config", http.MethodGet, nil, nil)
	if err == nil && d.AdditionVersion != 3 {
		if d.AdditionVersion < 2 {
			if !d.UseTransCodingAddress && len(d.DownProxyURL) == 0 {
				d.WebProxy = true
				d.WebdavPolicy = "native_proxy"
			}
		}
		// storages from before these two options get the old fixed values, so behavior is unchanged
		if d.DownConcurrency <= 0 {
			d.DownConcurrency = 3
		}
		if d.DownPartSize <= 0 {
			d.DownPartSize = 10
		}
		d.AdditionVersion = 3
		op.MustSaveDriverStorage(d)
	}
	// 定时刷新 __puus，避免会话 cookie 过期后下载 403（见 AlistGo/alist#830）
	d.startRefreshLoop()
	return err
}

func (d *QuarkOrUC) Drop(ctx context.Context) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	return nil
}

func (d *QuarkOrUC) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.GetFiles(dir.GetID())
	if err != nil {
		return nil, err
	}

	return files, nil
}

func (d *QuarkOrUC) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	f := file.(*File)
	if d.UseTransCodingAddress && d.config.Name == "Quark" && f.Category == 1 && f.Size > 0 {
		link, err := d.getTranscodingLink(file)
		if err == nil {
			return link, nil
		}
		if strings.Contains(err.Error(), "plf_invalid") {
			log.Warnf("quark transcoding link invalid for %s, fallback to download link: %v", file.GetName(), err)
			return d.getDownloadLink(file)
		}
		return nil, err
	}
	return d.getDownloadLink(file)
}

// mkdirConflictError keeps the Quark message and also reports as
// errs.ObjectAlreadyExists, which callers such as op.MakeDir tolerate.
type mkdirConflictError struct{ error }

func (e mkdirConflictError) Unwrap() error { return e.error }

func (mkdirConflictError) Is(target error) bool { return target == errs.ObjectAlreadyExists }

func (d *QuarkOrUC) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	data := base.Json{
		"dir_init_lock": false,
		"dir_path":      "",
		"file_name":     dirName,
		"pdir_fid":      parentDir.GetID(),
	}
	_, err := d.request("/file", http.MethodPost, func(req *resty.Request) {
		req.SetContext(ctx).SetBody(data)
	}, nil)
	if err != nil && err.Error() != mkdirConflictMessage {
		return nil, err
	}

	// Quark may acknowledge a mkdir before /file/sort reflects the new
	// directory. Confirm visibility and return the verified object so op.MakeDir
	// can update an existing parent cache directly instead of immediately
	// re-listing the eventually-consistent backend.
	obj, _, visibilityErr := d.waitForDirVisible(ctx, parentDir.GetID(), dirName)
	if visibilityErr == nil {
		return obj, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		// Preserve the existing Quark conflict error when the directory cannot
		// be confirmed. A same-name conflict is only treated as success after
		// the matching directory is visible in the parent listing.
		return nil, mkdirConflictError{err}
	}

	// The create request itself succeeded. A bounded visibility miss (whether
	// caused by clean-but-stale listings or listing errors) must not turn that
	// successful create into a false failure. Returning nil,nil makes
	// op.MakeDir clear the parent cache, so later reads cannot remain pinned to
	// a stale listing.
	return nil, nil
}

func (d *QuarkOrUC) waitForDirVisible(ctx context.Context, parentID, dirName string) (model.Obj, bool, error) {
	backoff := mkdirVisibilityInitialBackoff
	var lastErr error
	listed := false
	for attempt := 0; attempt < mkdirVisibilityAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, listed, err
		}

		obj, err := d.findDir(ctx, parentID, dirName)
		if err == nil {
			lastErr = nil
			listed = true
			if obj != nil {
				return obj, true, nil
			}
		} else {
			lastErr = err
		}
		if attempt == mkdirVisibilityAttempts-1 {
			break
		}

		select {
		case <-ctx.Done():
			return nil, listed, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < mkdirVisibilityMaxBackoff {
			backoff *= 2
			if backoff > mkdirVisibilityMaxBackoff {
				backoff = mkdirVisibilityMaxBackoff
			}
		}
	}

	if lastErr != nil {
		return nil, listed, fmt.Errorf("failed to confirm created directory %q: %w", dirName, lastErr)
	}
	return nil, listed, fmt.Errorf("created directory %q did not become visible after %d attempts", dirName, mkdirVisibilityAttempts)
}

func (d *QuarkOrUC) findDir(ctx context.Context, parentID, dirName string) (model.Obj, error) {
	const size = 100
	query := map[string]string{
		"pdir_fid":             parentID,
		"_size":                strconv.Itoa(size),
		"_fetch_total":         "1",
		"fetch_all_file":       "1",
		"fetch_risk_file_name": "1",
	}
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if page > mkdirVisibilityMaxPages {
			return nil, fmt.Errorf("quark mkdir visibility listing exceeded %d pages for parent %s", mkdirVisibilityMaxPages, parentID)
		}

		query["_page"] = strconv.Itoa(page)
		var resp SortResp
		_, err := d.request("/file/sort", http.MethodGet, func(req *resty.Request) {
			req.SetContext(ctx).SetQueryParams(query)
		}, &resp)
		if err != nil {
			return nil, err
		}
		for i := range resp.Data.List {
			file := &resp.Data.List[i]
			file.FileName = html.UnescapeString(file.FileName)
			if !file.File && file.FileName == dirName {
				return file, nil
			}
		}

		if resp.Metadata.Total > 0 {
			if page*size >= resp.Metadata.Total {
				return nil, nil
			}
		} else if len(resp.Data.List) < size {
			return nil, nil
		}
	}
}

func (d *QuarkOrUC) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	data := base.Json{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     []string{srcObj.GetID()},
		"to_pdir_fid":  dstDir.GetID(),
	}
	_, err := d.request("/file/move", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *QuarkOrUC) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	data := base.Json{
		"fid":       srcObj.GetID(),
		"file_name": newName,
	}
	_, err := d.request("/file/rename", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *QuarkOrUC) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NotSupport
}

func (d *QuarkOrUC) Remove(ctx context.Context, obj model.Obj) error {
	return d.removeReliable(ctx, obj)
}

// uploadedFileFromPre returns the exact FID allocated by upload/pre so op.Put
// can seed the parent cache after a successful upload. Quark directory listings
// can lag behind a completed upload; keeping this identity bridges that window
// for immediate follow-up operations such as WebDAV DELETE. If PRE did not
// provide a FID, preserve the old cache-clear behavior instead of inventing one.
func uploadedFileFromPre(pre UpPreResp, stream model.FileStreamer) model.Obj {
	if pre.Data.Fid == "" {
		return nil
	}
	now := time.Now().UnixMilli()
	return &File{
		Fid:        pre.Data.Fid,
		FileName:   stream.GetName(),
		Size:       stream.GetSize(),
		File:       true,
		LCreatedAt: now,
		LUpdatedAt: now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func (d *QuarkOrUC) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	md5Str, sha1Str := stream.GetHash().GetHash(utils.MD5), stream.GetHash().GetHash(utils.SHA1)
	var (
		md5  hash.Hash
		sha1 hash.Hash
	)
	writers := []io.Writer{}
	if len(md5Str) != utils.MD5.Width {
		md5 = utils.MD5.NewFunc()
		writers = append(writers, md5)
	}
	if len(sha1Str) != utils.SHA1.Width {
		sha1 = utils.SHA1.NewFunc()
		writers = append(writers, sha1)
	}

	if len(writers) > 0 {
		_, err := stream.CacheFullAndWriter(&up, io.MultiWriter(writers...))
		if err != nil {
			return nil, err
		}
		if md5 != nil {
			md5Str = hex.EncodeToString(md5.Sum(nil))
		}
		if sha1 != nil {
			sha1Str = hex.EncodeToString(sha1.Sum(nil))
		}
	}
	// pre
	pre, err := d.upPreReliable(ctx, stream, dstDir.GetID())
	if err != nil {
		return nil, err
	}
	uploadedObj := uploadedFileFromPre(pre, stream)
	log.Debugln("hash: ", md5Str, sha1Str)
	// hash
	finish, err := d.upHashReliable(ctx, md5Str, sha1Str, pre.Data.TaskId)
	if err != nil {
		return nil, err
	}
	if finish {
		return uploadedObj, nil
	}
	// part up
	ss, err := streamPkg.NewStreamSectionReader(stream, pre.Metadata.PartSize, &up)
	if err != nil {
		return nil, err
	}
	total := stream.GetSize()
	partSize := int64(pre.Metadata.PartSize)
	uploadNums := int((total + partSize - 1) / partSize)
	md5s := make([]string, 0, uploadNums)
	for partIndex := range uploadNums {
		if utils.IsCanceled(ctx) {
			return nil, ctx.Err()
		}
		offset := int64(partIndex) * partSize
		size := min(partSize, total-offset)
		rd, err := ss.GetSectionReader(offset, size)
		if err != nil {
			return nil, err
		}
		var m string
		err = retry.Do(func() error {
			var e error
			m, e = d.upPartReliable(ctx, pre, stream.GetMimetype(), partIndex+1, rd)
			return e
		},
			retry.Context(ctx),
			retry.Attempts(3),
			retry.DelayType(retry.BackOffDelay),
			retry.Delay(time.Second))
		ss.FreeSectionReader(rd)
		if err != nil {
			return nil, err
		}
		if m == "finish" {
			up(100)
			return uploadedObj, nil
		}
		md5s = append(md5s, m)
		up(95 * float64(offset+size) / float64(total))
	}
	up(97)
	if err := d.upCommitReliable(ctx, pre, md5s); err != nil {
		return nil, err
	}
	if err := d.upFinishReliable(ctx, pre, dstDir.GetID(), stream.GetName(), stream.GetSize()); err != nil {
		return nil, err
	}
	up(100)
	return uploadedObj, nil
}

func (d *QuarkOrUC) GetDetails(ctx context.Context) (*model.StorageDetails, error) {
	memberInfo, err := d.memberInfo(ctx)
	if err != nil {
		return nil, err
	}
	return &model.StorageDetails{
		DiskUsage: model.DiskUsage{
			TotalSpace: memberInfo.Data.TotalCapacity,
			UsedSpace:  memberInfo.Data.UseCapacity,
		},
	}, nil
}

var _ driver.Driver = (*QuarkOrUC)(nil)
