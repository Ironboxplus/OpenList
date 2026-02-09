package google_drive

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/avast/retry-go"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

// mkdirLocks prevents race conditions when creating folders with the same name
// Google Drive allows duplicate folder names, so we need application-level locking
var mkdirLocks sync.Map // map[string]*sync.Mutex - key is parentID + "/" + dirName

type GoogleDrive struct {
	model.Storage
	Addition
	AccessToken            string
	ServiceAccountFile     int
	ServiceAccountFileList []string
}

func (d *GoogleDrive) Config() driver.Config {
	return config
}

func (d *GoogleDrive) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *GoogleDrive) Init(ctx context.Context) error {
	if d.ChunkSize == 0 {
		d.ChunkSize = 5
	}
	return d.refreshToken()
}

func (d *GoogleDrive) Drop(ctx context.Context) error {
	return nil
}

func (d *GoogleDrive) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(dir.GetID())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *GoogleDrive) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	url := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?includeItemsFromAllDrives=true&supportsAllDrives=true", file.GetID())
	_, err := d.request(url, http.MethodGet, nil, nil)
	if err != nil {
		return nil, err
	}
	link := model.Link{
		URL: url + "&alt=media&acknowledgeAbuse=true",
		Header: http.Header{
			"Authorization": []string{"Bearer " + d.AccessToken},
		},
	}
	return &link, nil
}

func (d *GoogleDrive) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	// Use per-folder lock to prevent concurrent creation of same folder
	// This is critical because Google Drive allows duplicate folder names
	lockKey := parentDir.GetID() + "/" + dirName
	lockVal, _ := mkdirLocks.LoadOrStore(lockKey, &sync.Mutex{})
	lock := lockVal.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	// Check if folder already exists with retry to handle API eventual consistency
	escapedDirName := strings.ReplaceAll(dirName, "'", "\\'")
	query := map[string]string{
		"q":      fmt.Sprintf("name='%s' and '%s' in parents and mimeType='application/vnd.google-apps.folder' and trashed=false", escapedDirName, parentDir.GetID()),
		"fields": "files(id)",
	}

	var existingFiles Files
	err := retry.Do(func() error {
		var checkErr error
		_, checkErr = d.request("https://www.googleapis.com/drive/v3/files", http.MethodGet, func(req *resty.Request) {
			req.SetQueryParams(query)
		}, &existingFiles)
		return checkErr
	},
		retry.Context(ctx),
		retry.Attempts(3),
		retry.DelayType(retry.BackOffDelay),
		retry.Delay(200*time.Millisecond),
	)

	// If query succeeded and folder exists, return success (idempotent)
	if err == nil && len(existingFiles.Files) > 0 {
		log.Debugf("[google_drive] Folder '%s' already exists in parent %s, skipping creation", dirName, parentDir.GetID())
		return nil
	}
	// If query failed, return error to prevent duplicate creation
	if err != nil {
		return fmt.Errorf("failed to check existing folder '%s': %w", dirName, err)
	}

	// Create new folder (only when confirmed folder doesn't exist)
	data := base.Json{
		"name":     dirName,
		"parents":  []string{parentDir.GetID()},
		"mimeType": "application/vnd.google-apps.folder",
	}

	var createErr error
	err = retry.Do(func() error {
		_, createErr = d.request("https://www.googleapis.com/drive/v3/files", http.MethodPost, func(req *resty.Request) {
			req.SetBody(data)
		}, nil)
		return createErr
	},
		retry.Context(ctx),
		retry.Attempts(3),
		retry.DelayType(retry.BackOffDelay),
		retry.Delay(500*time.Millisecond),
	)

	if err != nil {
		return err
	}

	// Wait for API eventual consistency before releasing lock
	// This helps prevent race conditions where a concurrent request
	// checks for folder existence before the newly created folder is visible
	// 500ms is needed because Google Drive API has significant sync delay
	time.Sleep(500 * time.Millisecond)

	return nil
}

func (d *GoogleDrive) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	query := map[string]string{
		"addParents":    dstDir.GetID(),
		"removeParents": "root",
	}
	url := "https://www.googleapis.com/drive/v3/files/" + srcObj.GetID()
	_, err := d.request(url, http.MethodPatch, func(req *resty.Request) {
		req.SetQueryParams(query)
	}, nil)
	return err
}

func (d *GoogleDrive) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	data := base.Json{
		"name": newName,
	}
	url := "https://www.googleapis.com/drive/v3/files/" + srcObj.GetID()
	_, err := d.request(url, http.MethodPatch, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *GoogleDrive) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NotSupport
}

func (d *GoogleDrive) Remove(ctx context.Context, obj model.Obj) error {
	url := "https://www.googleapis.com/drive/v3/files/" + obj.GetID()
	_, err := d.request(url, http.MethodDelete, nil, nil)
	return err
}

func (d *GoogleDrive) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) error {
	// 1. 准备MD5（用于完整性校验）
	md5Hash := file.GetHash().GetHash(utils.MD5)

	// 检查是否是可重复读取的流
	_, isSeekable := file.(*stream.SeekableStream)

	if isSeekable {
		// 可重复读取的流，使用 RangeRead 计算 hash，不缓存
		if len(md5Hash) != utils.MD5.Width {
			var err error
			md5Hash, err = stream.StreamHashFile(file, utils.MD5, 10, &up)
			if err != nil {
				return err
			}
			_ = md5Hash // MD5用于后续完整性校验（Google Drive会自动校验）
		}
	} else {
		// 不可重复读取的流（如 HTTP body）
		if len(md5Hash) != utils.MD5.Width {
			// 缓存整个文件并计算 MD5
			var err error
			_, md5Hash, err = stream.CacheFullAndHash(file, &up, utils.MD5)
			if err != nil {
				return err
			}
			_ = md5Hash // MD5用于后续完整性校验
		} else if file.GetFile() == nil {
			// 有 MD5 但没有缓存，需要缓存以支持后续 RangeRead
			_, err := file.CacheFullAndWriter(&up, nil)
			if err != nil {
				return err
			}
		}
	}

	// 2. 初始化可恢复上传会话
	obj := file.GetExist()
	var (
		e    Error
		url  string
		data base.Json
		res  *resty.Response
		err  error
	)
	if obj != nil {
		url = fmt.Sprintf("https://www.googleapis.com/upload/drive/v3/files/%s?uploadType=resumable&supportsAllDrives=true", obj.GetID())
		data = base.Json{}
	} else {
		data = base.Json{
			"name":    file.GetName(),
			"parents": []string{dstDir.GetID()},
		}
		url = "https://www.googleapis.com/upload/drive/v3/files?uploadType=resumable&supportsAllDrives=true"
	}
	req := base.NoRedirectClient.R().
		SetHeaders(map[string]string{
			"Authorization":           "Bearer " + d.AccessToken,
			"X-Upload-Content-Type":   file.GetMimetype(),
			"X-Upload-Content-Length": strconv.FormatInt(file.GetSize(), 10),
		}).
		SetError(&e).SetBody(data).SetContext(ctx)
	if obj != nil {
		res, err = req.Patch(url)
	} else {
		res, err = req.Post(url)
	}
	if err != nil {
		return err
	}
	if e.Error.Code != 0 {
		if e.Error.Code == 401 {
			err = d.refreshToken()
			if err != nil {
				return err
			}
			return d.Put(ctx, dstDir, file, up)
		}
		return fmt.Errorf("%s: %v", e.Error.Message, e.Error.Errors)
	}

	// 3. 上传文件内容
	putUrl := res.Header().Get("location")
	if file.GetSize() < d.ChunkSize*1024*1024 {
		// 小文件上传：使用 RangeRead 读取整个文件（避免消费已计算hash的stream）
		reader, err := file.RangeRead(http_range.Range{Start: 0, Length: file.GetSize()})
		if err != nil {
			return err
		}

		_, err = d.request(putUrl, http.MethodPut, func(req *resty.Request) {
			req.SetHeader("Content-Length", strconv.FormatInt(file.GetSize(), 10)).
				SetBody(driver.NewLimitedUploadStream(ctx, reader))
		}, nil)
		return err
	} else {
		// 大文件分片上传
		return d.chunkUpload(ctx, file, putUrl, up)
	}
}

func (d *GoogleDrive) GetDetails(ctx context.Context) (*model.StorageDetails, error) {
	if d.DisableDiskUsage {
		return nil, errs.NotImplement
	}
	about, err := d.getAbout(ctx)
	if err != nil {
		return nil, err
	}
	var total, used int64
	if about.StorageQuota.Limit == nil {
		total = 0
	} else {
		total, err = strconv.ParseInt(*about.StorageQuota.Limit, 10, 64)
		if err != nil {
			return nil, err
		}
	}
	used, err = strconv.ParseInt(about.StorageQuota.Usage, 10, 64)
	if err != nil {
		return nil, err
	}
	return &model.StorageDetails{
		DiskUsage: model.DiskUsage{
			TotalSpace: total,
			UsedSpace:  used,
		},
	}, nil
}

var _ driver.Driver = (*GoogleDrive)(nil)
