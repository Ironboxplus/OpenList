package baidu_netdisk

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/net"
	streamPkg "github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/errgroup"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/avast/retry-go"
	log "github.com/sirupsen/logrus"
)

// calculateHashesStream 流式计算文件的MD5哈希值
// 返回：文件MD5、前256KB的MD5、每个分片的MD5列表、StreamSectionReader
func (d *BaiduNetdisk) calculateHashesStream(
	ctx context.Context,
	stream model.FileStreamer,
	sliceSize int64,
	up *driver.UpdateProgress,
) (contentMd5 string, sliceMd5 string, blockList []string, ss streamPkg.StreamSectionReaderIF, err error) {
	streamSize := stream.GetSize()
	count := 1
	if streamSize > sliceSize {
		count = int((streamSize + sliceSize - 1) / sliceSize)
	}
	lastBlockSize := streamSize % sliceSize
	if lastBlockSize == 0 {
		lastBlockSize = sliceSize
	}

	// 创建 StreamSectionReader 用于流式读取
	ss, err = streamPkg.NewStreamSectionReader(stream, int(sliceSize), nil)
	if err != nil {
		return "", "", nil, nil, err
	}

	// 前256KB的MD5
	const SliceSize int64 = 256 * utils.KB
	blockList = make([]string, 0, count)
	fileMd5H := md5.New()
	sliceMd5H2 := md5.New()
	sliceWritten := int64(0)

	for i := 0; i < count; i++ {
		if utils.IsCanceled(ctx) {
			return "", "", nil, nil, ctx.Err()
		}

		offset := int64(i) * sliceSize
		length := sliceSize
		if i == count-1 {
			length = lastBlockSize
		}

		reader, err := ss.GetSectionReader(offset, length)
		if err != nil {
			return "", "", nil, nil, err
		}

		// 计算分片MD5
		sliceMd5Calc := md5.New()

		// 同时写入多个哈希计算器
		writers := []io.Writer{fileMd5H, sliceMd5Calc}
		if sliceWritten < SliceSize {
			remaining := SliceSize - sliceWritten
			writers = append(writers, utils.LimitWriter(sliceMd5H2, remaining))
		}

		reader.Seek(0, io.SeekStart)
		n, err := io.Copy(io.MultiWriter(writers...), reader)
		if err != nil {
			ss.FreeSectionReader(reader)
			return "", "", nil, nil, err
		}
		sliceWritten += n

		blockList = append(blockList, hex.EncodeToString(sliceMd5Calc.Sum(nil)))
		ss.FreeSectionReader(reader)

		// 更新进度（哈希计算占总进度的一小部分）
		if up != nil {
			progress := float64(i+1) * 10 / float64(count)
			(*up)(progress)
		}
	}

	return hex.EncodeToString(fileMd5H.Sum(nil)),
		hex.EncodeToString(sliceMd5H2.Sum(nil)),
		blockList, ss, nil
}

// uploadChunksStream 流式上传所有分片
func (d *BaiduNetdisk) uploadChunksStream(
	ctx context.Context,
	ss streamPkg.StreamSectionReaderIF,
	stream model.FileStreamer,
	precreateResp *PrecreateResp,
	path string,
	sliceSize int64,
	count int,
	up driver.UpdateProgress,
) error {
	streamSize := stream.GetSize()
	lastBlockSize := streamSize % sliceSize
	if lastBlockSize == 0 {
		lastBlockSize = sliceSize
	}

	// 使用 OrderedGroup 保证 Before 阶段有序
	thread := min(d.uploadThread, len(precreateResp.BlockList))
	threadG, upCtx := errgroup.NewOrderedGroupWithContext(ctx, thread,
		retry.Attempts(UPLOAD_RETRY_COUNT),
		retry.Delay(UPLOAD_RETRY_WAIT_TIME),
		retry.MaxDelay(UPLOAD_RETRY_MAX_WAIT_TIME),
		retry.DelayType(retry.BackOffDelay),
		retry.RetryIf(func(err error) bool {
			return !errors.Is(err, ErrUploadIDExpired)
		}),
		retry.LastErrorOnly(true))

	totalParts := len(precreateResp.BlockList)

	for i, partseq := range precreateResp.BlockList {
		if utils.IsCanceled(upCtx) {
			break
		}
		if partseq < 0 {
			continue
		}

		i, partseq := i, partseq
		offset := int64(partseq) * sliceSize
		size := sliceSize
		if partseq+1 == count {
			size = lastBlockSize
		}

		var reader io.ReadSeeker

		threadG.GoWithLifecycle(errgroup.Lifecycle{
			Before: func(ctx context.Context) error {
				var err error
				reader, err = ss.GetSectionReader(offset, size)
				return err
			},
			Do: func(ctx context.Context) error {
				reader.Seek(0, io.SeekStart)
				err := d.uploadSliceStream(ctx, precreateResp.UploadURL, path,
					precreateResp.Uploadid, partseq, stream.GetName(), reader, size)
				if err != nil {
					return err
				}
				precreateResp.BlockList[i] = -1
				// 进度从10%开始（前10%是哈希计算）
				progress := 10 + float64(threadG.Success()+1)*90/float64(totalParts+1)
				up(progress)
				return nil
			},
			After: func(err error) {
				ss.FreeSectionReader(reader)
			},
		})
	}

	return threadG.Wait()
}

// uploadSliceStream 上传单个分片（接受io.ReadSeeker）
func (d *BaiduNetdisk) uploadSliceStream(
	ctx context.Context,
	uploadUrl string,
	path string,
	uploadid string,
	partseq int,
	fileName string,
	reader io.ReadSeeker,
	size int64,
) error {
	params := map[string]string{
		"method":       "upload",
		"access_token": d.AccessToken,
		"type":         "tmpfile",
		"path":         path,
		"uploadid":     uploadid,
		"partseq":      strconv.Itoa(partseq),
	}

	b := bytes.NewBuffer(make([]byte, 0, bytes.MinRead))
	mw := multipart.NewWriter(b)
	_, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		return err
	}
	headSize := b.Len()
	err = mw.Close()
	if err != nil {
		return err
	}
	head := bytes.NewReader(b.Bytes()[:headSize])
	tail := bytes.NewReader(b.Bytes()[headSize:])
	rateLimitedRd := driver.NewLimitedUploadStream(ctx, io.MultiReader(head, reader, tail))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadUrl+"/rest/2.0/pcs/superfile2", rateLimitedRd)
	if err != nil {
		return err
	}
	query := req.URL.Query()
	for k, v := range params {
		query.Set(k, v)
	}
	req.URL.RawQuery = query.Encode()
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(b.Len()) + size

	client := net.NewHttpClient()
	if d.UploadSliceTimeout > 0 {
		client.Timeout = time.Second * time.Duration(d.UploadSliceTimeout)
	} else {
		client.Timeout = DEFAULT_UPLOAD_SLICE_TIMEOUT
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b.Reset()
	_, err = b.ReadFrom(resp.Body)
	if err != nil {
		return err
	}
	body := b.Bytes()
	respStr := string(body)
	log.Debugln(respStr)
	lower := strings.ToLower(respStr)
	// 合并 uploadid 过期检测逻辑
	if strings.Contains(lower, "uploadid") &&
		(strings.Contains(lower, "invalid") || strings.Contains(lower, "expired") || strings.Contains(lower, "not found")) {
		return ErrUploadIDExpired
	}

	errCode := utils.Json.Get(body, "error_code").ToInt()
	errNo := utils.Json.Get(body, "errno").ToInt()
	if errCode != 0 || errNo != 0 {
		return errs.NewErr(errs.StreamIncomplete, "error uploading to baidu, response=%s", respStr)
	}
	return nil
}