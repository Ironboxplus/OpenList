package quark_open

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	streamPkg "github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/avast/retry-go"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

type QuarkOpen struct {
	model.Storage
	Addition
	config  driver.Config
	conf    Conf
	limiter *rate.Limiter
}

// 速率限制常量：夸克开放平台限流，保守设置
const (
	quarkRateLimit = 2.0 // 每秒2个请求，避免限流
)

func (d *QuarkOpen) Config() driver.Config {
	return d.config
}

func (d *QuarkOpen) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *QuarkOpen) Init(ctx context.Context) error {
	// 初始化速率限制器
	d.limiter = rate.NewLimiter(rate.Limit(quarkRateLimit), 1)

	var resp UserInfoResp

	_, err := d.request(ctx, "/open/v1/user/info", http.MethodGet, nil, &resp)
	if err != nil {
		return err
	}

	if resp.Data.UserID != "" {
		d.conf.userId = resp.Data.UserID
	} else {
		return errors.New("failed to get user ID")
	}

	return err
}

// waitLimit 等待速率限制
func (d *QuarkOpen) waitLimit(ctx context.Context) error {
	if d.limiter != nil {
		return d.limiter.Wait(ctx)
	}
	return nil
}

func (d *QuarkOpen) Drop(ctx context.Context) error {
	return nil
}

func (d *QuarkOpen) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	if err := d.waitLimit(ctx); err != nil {
		return nil, err
	}
	files, err := d.GetFiles(ctx, dir.GetID())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *QuarkOpen) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if err := d.waitLimit(ctx); err != nil {
		return nil, err
	}
	data := base.Json{
		"fid": file.GetID(),
	}
	var resp FileLikeResp
	_, err := d.request(ctx, "/open/v1/file/get_download_url", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, &resp)
	if err != nil {
		return nil, err
	}

	return &model.Link{
		URL: resp.Data.DownloadURL,
		Header: http.Header{
			"Cookie": []string{d.generateAuthCookie()},
		},
		Concurrency: 3,
		PartSize:    10 * utils.MB,
	}, nil
}

func (d *QuarkOpen) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	data := base.Json{
		"dir_path": dirName,
		"pdir_fid": parentDir.GetID(),
	}
	_, err := d.request(ctx, "/open/v1/dir", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)

	return err
}

func (d *QuarkOpen) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	data := base.Json{
		"action_type": 1,
		"fid_list":    []string{srcObj.GetID()},
		"to_pdir_fid": dstDir.GetID(),
	}
	_, err := d.request(ctx, "/open/v1/file/move", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)

	return err
}

func (d *QuarkOpen) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	data := base.Json{
		"fid":           srcObj.GetID(),
		"file_name":     newName,
		"conflict_mode": "REUSE",
	}
	_, err := d.request(ctx, "/open/v1/file/rename", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)

	return err
}

func (d *QuarkOpen) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NotSupport
}

func (d *QuarkOpen) Remove(ctx context.Context, obj model.Obj) error {
	data := base.Json{
		"action_type": 1,
		"fid_list":    []string{obj.GetID()},
	}
	_, err := d.request(ctx, "/open/v1/file/delete", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)

	return err
}

func (d *QuarkOpen) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	if err := d.waitLimit(ctx); err != nil {
		return err
	}
	md5Str, sha1Str := stream.GetHash().GetHash(utils.MD5), stream.GetHash().GetHash(utils.SHA1)

	// 检查是否需要计算hash
	needMD5 := len(md5Str) != utils.MD5.Width
	needSHA1 := len(sha1Str) != utils.SHA1.Width

	if needMD5 || needSHA1 {
		// 检查是否为可重复读取的流
		_, isSeekable := stream.(*streamPkg.SeekableStream)

		if isSeekable {
			// 可重复读取的流，使用 RangeRead 一次性计算所有hash，避免重复读取
			var md5 hash.Hash
			var sha1 hash.Hash
			writers := []io.Writer{}

			if needMD5 {
				md5 = utils.MD5.NewFunc()
				writers = append(writers, md5)
			}
			if needSHA1 {
				sha1 = utils.SHA1.NewFunc()
				writers = append(writers, sha1)
			}

			// 使用 RangeRead 分块读取文件，同时计算多个hash
			multiWriter := io.MultiWriter(writers...)
			size := stream.GetSize()
			chunkSize := int64(10 * utils.MB) // 10MB per chunk
			buf := make([]byte, chunkSize)
			var offset int64 = 0

			for offset < size {
				readSize := min(chunkSize, size-offset)

				n, err := streamPkg.ReadFullWithRangeRead(stream, buf[:readSize], offset)
				if err != nil {
					return fmt.Errorf("calculate hash failed at offset %d: %w", offset, err)
				}

				multiWriter.Write(buf[:n])
				offset += int64(n)

				// 更新进度（hash计算占用40%的进度）
				up(40 * float64(offset) / float64(size))
			}

			if md5 != nil {
				md5Str = hex.EncodeToString(md5.Sum(nil))
			}
			if sha1 != nil {
				sha1Str = hex.EncodeToString(sha1.Sum(nil))
			}
		} else {
			// 不可重复读取的流（如网络流），需要缓存并计算hash
			var md5 hash.Hash
			var sha1 hash.Hash
			writers := []io.Writer{}

			if needMD5 {
				md5 = utils.MD5.NewFunc()
				writers = append(writers, md5)
			}
			if needSHA1 {
				sha1 = utils.SHA1.NewFunc()
				writers = append(writers, sha1)
			}

			_, err := stream.CacheFullAndWriter(&up, io.MultiWriter(writers...))
			if err != nil {
				return err
			}

			if md5 != nil {
				md5Str = hex.EncodeToString(md5.Sum(nil))
			}
			if sha1 != nil {
				sha1Str = hex.EncodeToString(sha1.Sum(nil))
			}
		}
	}
	// pre - 带有 proof fail 重试逻辑
	var pre UpPreResp
	var err error
	err = retry.Do(func() error {
		var preErr error
		pre, preErr = d.upPre(ctx, stream, dstDir.GetID(), md5Str, sha1Str)
		if preErr != nil {
			// 检查是否为 proof fail 错误
			if strings.Contains(preErr.Error(), "proof") || strings.Contains(preErr.Error(), "43010") {
				log.Warnf("[quark_open] Proof verification failed, retrying: %v", preErr)
				return preErr // 返回错误触发重试
			}
			// 检查是否为限流错误
			if strings.Contains(preErr.Error(), "限流") || strings.Contains(preErr.Error(), "rate") {
				log.Warnf("[quark_open] Rate limited, waiting before retry: %v", preErr)
				time.Sleep(2 * time.Second) // 额外等待
				return preErr
			}
		}
		return preErr
	},
		retry.Context(ctx),
		retry.Attempts(3),
		retry.DelayType(retry.BackOffDelay),
		retry.Delay(500*time.Millisecond),
	)
	if err != nil {
		return err
	}
	// 如果预上传已经完成，直接返回--秒传
	if pre.Data.Finish {
		up(100)
		return nil
	}

	// 空文件特殊处理：跳过分片上传，直接调用 upFinish
	// 由于夸克 API 对空文件处理不稳定，尝试完成上传，失败则直接成功返回
	if stream.GetSize() == 0 {
		log.Infof("[quark_open] Empty file detected, attempting direct finish (task_id: %s)", pre.Data.TaskID)
		err = d.upFinish(ctx, pre, []base.Json{}, []string{})
		if err != nil {
			// 空文件 upFinish 失败，可能是 API 不支持，直接视为成功
			log.Warnf("[quark_open] Empty file upFinish failed: %v, treating as success", err)
		}
		up(100)
		return nil
	}

	// 带重试的分片大小调整逻辑：如果检测到 "part list exceed" 错误，自动翻倍分片大小
	var upUrlInfo UpUrlInfo
	var partInfo []base.Json
	currentPartSize := pre.Data.PartSize
	const maxRetries = 5
	const maxPartSize = 1024 * utils.MB // 1GB 上限

	for attempt := 0; attempt < maxRetries; attempt++ {
		// 计算分片信息
		partInfo = d._getPartInfo(stream, currentPartSize)

		// 尝试获取上传 URL
		upUrlInfo, err = d.upUrl(ctx, pre, partInfo)
		if err == nil {
			// 成功获取上传 URL
			log.Infof("[quark_open] Successfully obtained upload URLs with part size: %d MB (%d parts)",
				currentPartSize/(1024*1024), len(partInfo))
			break
		}

		// 检查是否为分片超限错误
		if strings.Contains(err.Error(), "exceed") {
			if attempt < maxRetries-1 {
				// 还有重试机会，翻倍分片大小
				newPartSize := currentPartSize * 2

				// 检查是否超过上限
				if newPartSize > maxPartSize {
					return fmt.Errorf("part list exceeded and cannot increase part size (current: %d MB, max: %d MB). File may be too large for Quark API",
						currentPartSize/(1024*1024), maxPartSize/(1024*1024))
				}

				log.Warnf("[quark_open] Part list exceeded (attempt %d/%d, %d parts). Retrying with doubled part size: %d MB -> %d MB",
					attempt+1, maxRetries, len(partInfo),
					currentPartSize/(1024*1024), newPartSize/(1024*1024))

				currentPartSize = newPartSize
				continue // 重试
			} else {
				// 已达到最大重试次数
				return fmt.Errorf("part list exceeded after %d retries. Last attempt: part size %d MB, %d parts",
					maxRetries, currentPartSize/(1024*1024), len(partInfo))
			}
		}

		// 其他错误，直接返回
		return err
	}

	// part up - 使用调整后的 currentPartSize
	ss, err := streamPkg.NewStreamSectionReader(stream, int(currentPartSize), &up)
	if err != nil {
		return err
	}
	total := stream.GetSize()
	// 用于存储每个分片的ETag，后续commit时需要
	etags := make([]string, 0, len(partInfo))

	// 遍历上传每个分片
	for i := range len(upUrlInfo.UploadUrls) {
		if utils.IsCanceled(ctx) {
			return ctx.Err()
		}

		offset := int64(i) * currentPartSize
		size := min(currentPartSize, total-offset)
		rd, err := ss.GetSectionReader(offset, size)
		if err != nil {
			return err
		}

		// 上传重试逻辑，包含URL刷新
		var etag string
		err = retry.Do(func() error {
			rd.Seek(0, io.SeekStart)
			var uploadErr error
			etag, uploadErr = d.upPart(ctx, upUrlInfo, i, driver.NewLimitedUploadStream(ctx, rd))

			// 检查是否为URL过期错误
			if uploadErr != nil && strings.Contains(uploadErr.Error(), "expire") {
				log.Warnf("[quark_open] Upload URL expired for part %d, refreshing...", i)
				// 刷新上传URL
				newUpUrlInfo, refreshErr := d.upUrl(ctx, pre, partInfo)
				if refreshErr != nil {
					return fmt.Errorf("failed to refresh upload url: %w", refreshErr)
				}
				upUrlInfo = newUpUrlInfo
				log.Infof("[quark_open] Upload URL refreshed successfully")

				// 使用新URL重试上传
				rd.Seek(0, io.SeekStart)
				etag, uploadErr = d.upPart(ctx, upUrlInfo, i, driver.NewLimitedUploadStream(ctx, rd))
			}

			return uploadErr
		},
			retry.Context(ctx),
			retry.Attempts(3),
			retry.DelayType(retry.BackOffDelay),
			retry.Delay(time.Second))

		ss.FreeSectionReader(rd)
		if err != nil {
			return fmt.Errorf("failed to upload part %d: %w", i, err)
		}

		etags = append(etags, etag)
		up(95 * float64(offset+size) / float64(total))
	}

	defer up(100)
	return d.upFinish(ctx, pre, partInfo, etags)
}

var _ driver.Driver = (*QuarkOpen)(nil)
