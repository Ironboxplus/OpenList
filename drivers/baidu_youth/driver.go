package baidu_youth

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	stdpath "path"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/errgroup"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/avast/retry-go"
	"github.com/go-resty/resty/v2"
)

type BaiduYouth struct {
	model.Storage
	Addition

	uk           int64
	bdstoken     string
	sk           string
	uploadThread int
	upClient     *resty.Client
}

var ErrUploadIDExpired = errors.New("uploadid expired")

func (d *BaiduYouth) Config() driver.Config {
	return config
}

func (d *BaiduYouth) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *BaiduYouth) Init(ctx context.Context) error {
	d.Cookie = strings.TrimSpace(d.Cookie)
	if d.Storage.Addition != "" {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(d.Storage.Addition), &raw); err == nil {
			if _, ok := raw["force_proxy"]; !ok {
				d.ForceProxy = true
			}
		}
	}
	d.upClient = base.NewRestyClient().
		SetTimeout(UPLOAD_TIMEOUT).
		SetRetryCount(UPLOAD_RETRY_COUNT).
		SetRetryWaitTime(UPLOAD_RETRY_WAIT_TIME).
		SetRetryMaxWaitTime(UPLOAD_RETRY_MAX_WAIT_TIME)

	d.uploadThread, _ = strconv.Atoi(d.UploadThread)
	if d.uploadThread < 1 {
		d.uploadThread, d.UploadThread = 1, "1"
	} else if d.uploadThread > 32 {
		d.uploadThread, d.UploadThread = 32, "32"
	}

	u, err := url.Parse(d.UploadAPI)
	if d.UploadAPI == "" || err != nil || u.Scheme == "" || u.Host == "" {
		d.UploadAPI = UPLOAD_FALLBACK_API
	} else {
		d.UploadAPI = strings.TrimRight(d.UploadAPI, "/")
	}

	uk, bdstoken, sk, err := d.getUserSession(ctx)
	if err != nil {
		return err
	}
	d.uk = uk
	d.bdstoken = bdstoken
	d.sk = sk
	return nil
}

func (d *BaiduYouth) ShouldProxyDownloads() bool {
	return d.ForceProxy
}

func (d *BaiduYouth) Drop(ctx context.Context) error {
	d.uk = 0
	d.bdstoken = ""
	d.sk = ""
	return nil
}

func (d *BaiduYouth) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(ctx, dir.GetPath())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *BaiduYouth) Get(ctx context.Context, path string) (model.Obj, error) {
	return d.getByPath(ctx, path)
}

func (d *BaiduYouth) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if file.IsDir() {
		return nil, errs.NotFile
	}
	if d.DownloadAPI == "crack" {
		return d.linkCrack(ctx, file)
	}
	return d.linkOfficial(ctx, file)
}

func (d *BaiduYouth) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	path := stdpath.Join(parentDir.GetPath(), dirName)
	var resp CreateResp
	_, err := d.postForm(ctx, "/youth/api/create", map[string]string{
		"a":        "commit",
		"bdstoken": d.bdstoken,
	}, map[string]string{
		"block_list": "[]",
		"isdir":      "1",
		"path":       path,
	}, &resp)
	if err != nil {
		return nil, err
	}
	if result := resp.ResultFile(); result.Path != "" || result.FsId != 0 {
		return fileToObj(result), nil
	}
	return d.getByPath(ctx, path)
}

func (d *BaiduYouth) Move(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	_, err := d.manage(ctx, "move", []base.Json{
		{
			"dest":    dstDir.GetPath(),
			"newname": srcObj.GetName(),
			"path":    srcObj.GetPath(),
		},
	})
	if err != nil {
		return nil, err
	}
	newPath := stdpath.Join(dstDir.GetPath(), srcObj.GetName())
	if obj, ok := srcObj.(*model.ObjThumb); ok {
		obj.SetPath(newPath)
		obj.Modified = time.Now()
		return obj, nil
	}
	return d.getByPath(ctx, newPath)
}

func (d *BaiduYouth) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	_, err := d.manage(ctx, "rename", []base.Json{
		{
			"id":      srcObj.GetID(),
			"newname": newName,
			"path":    srcObj.GetPath(),
		},
	})
	if err != nil {
		return nil, err
	}
	newPath := stdpath.Join(stdpath.Dir(srcObj.GetPath()), newName)
	if obj, ok := srcObj.(*model.ObjThumb); ok {
		obj.SetPath(newPath)
		obj.Name = newName
		obj.Modified = time.Now()
		return obj, nil
	}
	return d.getByPath(ctx, newPath)
}

func (d *BaiduYouth) Copy(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	newPath := stdpath.Join(dstDir.GetPath(), srcObj.GetName())
	_, err := d.manage(ctx, "copy", []base.Json{
		{
			"dest":    dstDir.GetPath(),
			"newname": srcObj.GetName(),
			"path":    srcObj.GetPath(),
		},
	})
	if err != nil {
		return nil, err
	}
	if obj, ok := srcObj.(*model.ObjThumb); ok {
		copied := *obj
		copied.SetPath(newPath)
		copied.Modified = time.Now()
		return &copied, nil
	}
	// Youth copy returns success before /api/filemetas can resolve the new path.
	// Avoid turning a successful copy into a false failure because the immediate
	// post-copy lookup is temporarily unavailable.
	return &model.Object{
		ID:       newPath,
		Path:     newPath,
		Name:     srcObj.GetName(),
		Size:     srcObj.GetSize(),
		Modified: time.Now(),
		Ctime:    srcObj.CreateTime(),
		IsFolder: srcObj.IsDir(),
		HashInfo: srcObj.GetHash(),
	}, nil
}

func (d *BaiduYouth) Remove(ctx context.Context, obj model.Obj) error {
	_, err := d.manage(ctx, "delete", []string{obj.GetPath()})
	return err
}

func (d *BaiduYouth) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	if stream.GetSize() < 1 {
		return nil, ErrBaiduYouthEmptyFilesNotAllowed
	}

	var (
		cache = stream.GetFile()
		tmpF  *os.File
		err   error
	)
	if _, ok := cache.(io.ReaderAt); !ok {
		tmpF, err = os.CreateTemp(conf.Conf.TempDir, "file-*")
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = tmpF.Close()
			_ = os.Remove(tmpF.Name())
		}()
		cache = tmpF
	}

	streamSize := stream.GetSize()
	sliceSize := DefaultSliceSize
	count := int(streamSize / sliceSize)
	lastBlockSize := streamSize % sliceSize
	if lastBlockSize > 0 {
		count++
	} else {
		lastBlockSize = sliceSize
	}

	const sliceMD5Size int64 = 256 * utils.KB
	blockList := make([]string, 0, count)
	byteSize := sliceSize
	fileMd5H := md5.New()
	sliceMd5H := md5.New()
	sliceMd5H2 := md5.New()
	sliceMd5H2Writer := utils.LimitWriter(sliceMd5H2, sliceMD5Size)
	writers := []io.Writer{fileMd5H, sliceMd5H, sliceMd5H2Writer}
	if tmpF != nil {
		writers = append(writers, tmpF)
	}

	written := int64(0)
	for i := 1; i <= count; i++ {
		if utils.IsCanceled(ctx) {
			return nil, ctx.Err()
		}
		if i == count {
			byteSize = lastBlockSize
		}
		n, err := utils.CopyWithBufferN(io.MultiWriter(writers...), stream, byteSize)
		written += n
		if err != nil && err != io.EOF {
			return nil, err
		}
		blockList = append(blockList, hex.EncodeToString(sliceMd5H.Sum(nil)))
		sliceMd5H.Reset()
	}

	if tmpF != nil {
		if written != streamSize {
			return nil, errs.NewErr(errs.StreamIncomplete, "temp file size mismatch: %d != %d", written, streamSize)
		}
		if _, err = tmpF.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}

	contentMd5 := hex.EncodeToString(fileMd5H.Sum(nil))
	sliceMd5 := hex.EncodeToString(sliceMd5H2.Sum(nil))
	blockListStr, err := utils.Json.MarshalToString(blockList)
	if err != nil {
		return nil, err
	}
	path := stdpath.Join(dstDir.GetPath(), stream.GetName())
	mtime := stream.ModTime().Unix()
	ctime := stream.CreateTime().Unix()

	progressKey := d.uploadProgressKey()
	precreateResp, ok := base.GetUploadProgress[*PrecreateResp](d, progressKey, contentMd5)
	if !ok {
		precreateResp, err = d.precreate(ctx, path, streamSize, blockListStr, contentMd5, sliceMd5, ctime, mtime)
		if err != nil {
			return nil, err
		}
	}

	if precreateResp.ReturnType >= 2 {
		result := precreateResp.ResultFile()
		if result.Path == "" && result.FsId == 0 {
			return d.getByPath(ctx, path)
		}
		result.Ctime = ctime
		result.Mtime = mtime
		return fileToObj(result), nil
	}

	cacheReaderAt, ok := cache.(io.ReaderAt)
	if !ok {
		return nil, fmt.Errorf("cache object must implement io.ReaderAt")
	}

uploadLoop:
	for attempt := 0; attempt < 2; attempt++ {
		completed := count - len(precreateResp.BlockList)
		threadG, upCtx := errgroup.NewGroupWithContext(ctx, d.uploadThread,
			retry.Attempts(1),
			retry.Delay(time.Second),
			retry.DelayType(retry.BackOffDelay))

		for i, partseq := range precreateResp.BlockList {
			if utils.IsCanceled(upCtx) || partseq < 0 {
				continue
			}

			i, partseq := i, partseq
			offset, size := int64(partseq)*sliceSize, sliceSize
			if partseq+1 == count {
				size = lastBlockSize
			}

			threadG.Go(func(ctx context.Context) error {
				params := map[string]string{
					"method":   "upload",
					"partseq":  strconv.Itoa(partseq),
					"path":     path,
					"type":     "tmpfile",
					"uploadid": precreateResp.Uploadid,
				}
				if precreateResp.Uploadsign != "" {
					params["uploadsign"] = precreateResp.Uploadsign
				}
				section := io.NewSectionReader(cacheReaderAt, offset, size)
				if err := d.uploadSlice(ctx, params, stream.GetName(), driver.NewLimitedUploadStream(ctx, section)); err != nil {
					return err
				}
				precreateResp.BlockList[i] = -1
				success := completed + int(threadG.Success()) + 1
				up(float64(success) * 100 / float64(count))
				return nil
			})
		}

		err = threadG.Wait()
		if err == nil {
			break uploadLoop
		}

		precreateResp.BlockList = utils.SliceFilter(precreateResp.BlockList, func(part int) bool {
			return part >= 0
		})
		base.SaveUploadProgress(d, precreateResp, progressKey, contentMd5)

		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if errors.Is(err, ErrUploadIDExpired) {
			precreateResp, err = d.precreate(ctx, path, streamSize, blockListStr, contentMd5, sliceMd5, ctime, mtime)
			if err != nil {
				return nil, err
			}
			if precreateResp.ReturnType >= 2 {
				result := precreateResp.ResultFile()
				if result.Path == "" && result.FsId == 0 {
					return d.getByPath(ctx, path)
				}
				result.Ctime = ctime
				result.Mtime = mtime
				return fileToObj(result), nil
			}
			base.SaveUploadProgress(d, precreateResp, progressKey, contentMd5)
			continue uploadLoop
		}
		return nil, err
	}

	var createResp CreateResp
	_, err = d.createFile(ctx, path, stdpath.Dir(path), streamSize, precreateResp.Uploadid, precreateResp.Uploadsign, blockListStr, &createResp, mtime, ctime)
	if err != nil {
		return nil, err
	}

	base.SaveUploadProgress(d, nil, progressKey, contentMd5)
	result := createResp.ResultFile()
	if result.Path == "" && result.FsId == 0 {
		return d.getByPath(ctx, path)
	}
	result.Ctime = ctime
	result.Mtime = mtime
	return fileToObj(result), nil
}

var _ driver.Driver = (*BaiduYouth)(nil)
var _ driver.Getter = (*BaiduYouth)(nil)
var _ driver.MkdirResult = (*BaiduYouth)(nil)
var _ driver.MoveResult = (*BaiduYouth)(nil)
var _ driver.RenameResult = (*BaiduYouth)(nil)
var _ driver.CopyResult = (*BaiduYouth)(nil)
var _ driver.Remove = (*BaiduYouth)(nil)
var _ driver.PutResult = (*BaiduYouth)(nil)
