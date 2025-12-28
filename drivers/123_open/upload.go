package _123_open

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/errgroup"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/avast/retry-go"
	"github.com/go-resty/resty/v2"
)

// 创建文件 V2
func (d *Open123) create(parentFileID int64, filename string, etag string, size int64, duplicate int, containDir bool) (*UploadCreateResp, error) {
	utils.Log.Debugf("    ┌─ [create API] 调用开始 ─────────────────────────┐")
	utils.Log.Debugf("    │ URL: %s", UploadCreate)

	lowerEtag := strings.ToLower(etag)
	utils.Log.Debugf("    │ 请求参数:")
	utils.Log.Debugf("    │   parentFileId: %d", parentFileID)
	utils.Log.Debugf("    │   filename: %s", filename)
	utils.Log.Debugf("    │   etag(原始): %s", etag)
	utils.Log.Debugf("    │   etag(小写): %s", lowerEtag)
	utils.Log.Debugf("    │   size: %d", size)
	utils.Log.Debugf("    │   duplicate: %d", duplicate)
	utils.Log.Debugf("    │   containDir: %v", containDir)

	var resp UploadCreateResp
	_, err := d.Request(UploadCreate, http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"parentFileId": parentFileID,
			"filename":     filename,
			"etag":         lowerEtag,
			"size":         size,
			"duplicate":    duplicate,
			"containDir":   containDir,
		})
	}, &resp)

	if err != nil {
		utils.Log.Errorf("    │ ✗ API调用失败: %v", err)
		utils.Log.Debugf("    └───────────────────────────────────────────────┘")
		return nil, err
	}

	utils.Log.Debugf("    │ ✓ API调用成功")
	utils.Log.Debugf("    │ 响应:")
	utils.Log.Debugf("    │   Code: %d", resp.Code)
	utils.Log.Debugf("    │   Message: %s", resp.Message)
	utils.Log.Debugf("    │   Data.Reuse: %v", resp.Data.Reuse)
	utils.Log.Debugf("    │   Data.FileID: %d", resp.Data.FileID)
	utils.Log.Debugf("    │   Data.PreuploadID: %s", resp.Data.PreuploadID)
	utils.Log.Debugf("    │   Data.SliceSize: %d", resp.Data.SliceSize)
	if len(resp.Data.Servers) > 0 {
		utils.Log.Debugf("    │   Data.Servers[0]: %s", resp.Data.Servers[0])
	}
	utils.Log.Debugf("    └───────────────────────────────────────────────┘")

	return &resp, nil
}

// 上传分片 V2
func (d *Open123) Upload(ctx context.Context, file model.FileStreamer, createResp *UploadCreateResp, up driver.UpdateProgress) error {
	uploadDomain := createResp.Data.Servers[0]
	size := file.GetSize()
	chunkSize := createResp.Data.SliceSize

	ss, err := stream.NewStreamSectionReader(file, int(chunkSize), &up)
	if err != nil {
		return err
	}

	uploadNums := (size + chunkSize - 1) / chunkSize
	thread := min(int(uploadNums), d.UploadThread)
	threadG, uploadCtx := errgroup.NewOrderedGroupWithContext(ctx, thread,
		retry.Attempts(3),
		retry.Delay(time.Second),
		retry.DelayType(retry.BackOffDelay))

	for partIndex := range uploadNums {
		if utils.IsCanceled(uploadCtx) {
			break
		}
		partIndex := partIndex
		partNumber := partIndex + 1 // 分片号从1开始
		offset := partIndex * chunkSize
		size := min(chunkSize, size-offset)
		var reader io.ReadSeeker
		var rateLimitedRd io.Reader
		sliceMD5 := ""
		// 表单
		b := bytes.NewBuffer(make([]byte, 0, 2048))
		threadG.GoWithLifecycle(errgroup.Lifecycle{
			Before: func(ctx context.Context) (err error) {
				reader, err = ss.GetSectionReader(offset, size)
				return
			},
			Do: func(ctx context.Context) (err error) {
				reader.Seek(0, io.SeekStart)
				if sliceMD5 == "" {
					// 把耗时的计算放在这里，避免阻塞其他协程
					sliceMD5, err = utils.HashReader(utils.MD5, reader)
					if err != nil {
						return err
					}
					reader.Seek(0, io.SeekStart)
				}

				b.Reset()
				w := multipart.NewWriter(b)
				// 添加表单字段
				err = w.WriteField("preuploadID", createResp.Data.PreuploadID)
				if err != nil {
					return err
				}
				err = w.WriteField("sliceNo", strconv.FormatInt(partNumber, 10))
				if err != nil {
					return err
				}
				err = w.WriteField("sliceMD5", sliceMD5)
				if err != nil {
					return err
				}
				// 写入文件内容
				_, err = w.CreateFormFile("slice", fmt.Sprintf("%s.part%d", file.GetName(), partNumber))
				if err != nil {
					return err
				}
				headSize := b.Len()
				err = w.Close()
				if err != nil {
					return err
				}
				head := bytes.NewReader(b.Bytes()[:headSize])
				tail := bytes.NewReader(b.Bytes()[headSize:])
				rateLimitedRd = driver.NewLimitedUploadStream(ctx, io.MultiReader(head, reader, tail))
				// 创建请求并设置header
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadDomain+"/upload/v2/file/slice", rateLimitedRd)
				if err != nil {
					return err
				}

				// 设置请求头
				req.Header.Add("Authorization", "Bearer "+d.AccessToken)
				req.Header.Add("Content-Type", w.FormDataContentType())
				req.Header.Add("Platform", "open_platform")

				res, err := base.HttpClient.Do(req)
				if err != nil {
					return err
				}
				defer res.Body.Close()
				if res.StatusCode != 200 {
					return fmt.Errorf("slice %d upload failed, status code: %d", partNumber, res.StatusCode)
				}
				b.Reset()
				_, err = b.ReadFrom(res.Body)
				if err != nil {
					return err
				}
				var resp BaseResp
				err = json.Unmarshal(b.Bytes(), &resp)
				if err != nil {
					return err
				}
				if resp.Code != 0 {
					return fmt.Errorf("slice %d upload failed: %s", partNumber, resp.Message)
				}

				progress := 100 * float64(threadG.Success()+1) / float64(uploadNums+1)
				up(progress)
				return nil
			},
			After: func(err error) {
				ss.FreeSectionReader(reader)
			},
		})
	}

	if err := threadG.Wait(); err != nil {
		return err
	}

	return nil
}

// 上传完毕
func (d *Open123) complete(preuploadID string) (*UploadCompleteResp, error) {
	utils.Log.Debugf("    ┌─ [complete API] 调用 ────────────────────────┐")
	utils.Log.Debugf("    │ URL: %s", UploadComplete)
	utils.Log.Debugf("    │ 请求参数:")
	utils.Log.Debugf("    │   preuploadID: %s", preuploadID)

	var resp UploadCompleteResp
	_, err := d.Request(UploadComplete, http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"preuploadID": preuploadID,
		})
	}, &resp)

	if err != nil {
		utils.Log.Debugf("    │ ✗ API调用失败: %v", err)
		utils.Log.Debugf("    └──────────────────────────────────────────────┘")
		return nil, err
	}

	utils.Log.Debugf("    │ ✓ API响应:")
	utils.Log.Debugf("    │   Code: %d", resp.Code)
	utils.Log.Debugf("    │   Message: %s", resp.Message)
	utils.Log.Debugf("    │   Data.Completed: %v", resp.Data.Completed)
	utils.Log.Debugf("    │   Data.FileID: %d", resp.Data.FileID)
	utils.Log.Debugf("    └──────────────────────────────────────────────┘")

	return &resp, nil
}
