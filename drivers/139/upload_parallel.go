package _139

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils/random"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// 官方客户端在开启 parallelUpload 时，会为每个分片附带上传到该分片起点时的
// SHA256 中间状态。服务端把它签进分片的上传地址（X-Amz-Iteration-Hash-Ctx），
// 存储节点据此独立校验该分片，因此分片可以乱序并发上传。
//
// 这里单独定义一套请求结构，避免影响现有串行上传路径的报文。
type parallelHashCtx struct {
	// H 是 SHA256 的 8 个状态寄存器
	H          []uint32 `json:"h"`
	PartOffset int64    `json:"partOffset"`
}

type parallelPartInfo struct {
	PartNumber int64 `json:"partNumber"`
	PartSize   int64 `json:"partSize"`
	// 第一个分片从 SHA256 初始向量开始，不携带上下文
	ParallelHashCtx *parallelHashCtx `json:"parallelHashCtx,omitempty"`

	offset int64
}

// sha256MidstateLen 是 crypto/sha256 序列化状态的最小长度：4 字节 magic +
// 8 个状态寄存器 + 末尾 8 字节长度。
const sha256MidstateLen = 4 + 8*4 + 8

// sha256Midstate 导出 h 当前的 8 个状态寄存器，以及已经写入的字节数。
func sha256Midstate(h hash.Hash) ([]uint32, int64, error) {
	m, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil, 0, errors.New("sha256 hash state is not exportable")
	}
	state, err := m.MarshalBinary()
	if err != nil {
		return nil, 0, err
	}
	if len(state) < sha256MidstateLen {
		return nil, 0, fmt.Errorf("unexpected sha256 state length %d", len(state))
	}
	regs := make([]uint32, 8)
	for i := range regs {
		regs[i] = binary.BigEndian.Uint32(state[4+i*4:])
	}
	return regs, int64(binary.BigEndian.Uint64(state[len(state)-8:])), nil
}

// planParallelParts 顺序扫描一遍文件，同时得到整文件 SHA256 和每个分片起点的
// 哈希中间状态。
func planParallelParts(f model.File, size, partSize int64) (string, []parallelPartInfo, error) {
	// 分片边界必须落在 SHA256 的分组边界上，否则未满一组的字节还留在缓冲区里，
	// 只导出状态寄存器会把它们丢掉。
	if partSize%64 != 0 {
		return "", nil, fmt.Errorf("part size %d is not a multiple of 64 bytes", partSize)
	}
	h := sha256.New()
	buf := make([]byte, 1024*1024)
	var partInfos []parallelPartInfo

	for offset, partNumber := int64(0), int64(1); offset < size; partNumber++ {
		byteSize := size - offset
		if byteSize > partSize {
			byteSize = partSize
		}
		partInfo := parallelPartInfo{
			PartNumber: partNumber,
			PartSize:   byteSize,
			offset:     offset,
		}
		if partNumber > 1 {
			regs, hashed, err := sha256Midstate(h)
			if err != nil {
				return "", nil, err
			}
			// 分片大小不是 64 的整数倍时中间状态无法只用寄存器表达，此处再校验一次
			if hashed != offset {
				return "", nil, fmt.Errorf("sha256 state covers %d bytes, expected %d", hashed, offset)
			}
			partInfo.ParallelHashCtx = &parallelHashCtx{H: regs, PartOffset: offset}
		}
		partInfos = append(partInfos, partInfo)

		if _, err := io.CopyBuffer(h, io.NewSectionReader(f, offset, byteSize), buf); err != nil {
			return "", nil, err
		}
		offset += byteSize
	}
	if len(partInfos) == 0 {
		partInfos = append(partInfos, parallelPartInfo{PartNumber: 1})
	}
	return hex.EncodeToString(h.Sum(nil)), partInfos, nil
}

// parallelProgress 汇总所有并发分片已上传的字节数。回调本身不保证并发安全，
// 因此上报要串行化。
type parallelProgress struct {
	total int64
	done  int64
	mu    sync.Mutex
	up    driver.UpdateProgress
}

func (p *parallelProgress) add(n int64) {
	if p.total <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += n
	percentage := float64(p.done) / float64(p.total) * 100
	if percentage > 100 {
		percentage = 100
	}
	p.up(percentage)
}

type parallelProgressReader struct {
	io.Reader
	progress *parallelProgress
}

func (r *parallelProgressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.progress.add(int64(n))
	}
	return n, err
}

// putPersonalNewParallel 走官方客户端的并发上传流程，仅在 parallel_upload
// 打开时使用。串行上传逻辑保持不变。
func (d *Yun139) putPersonalNewParallel(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	size := stream.GetSize()
	partSize := d.getPartSize(size)
	if partSize%64 != 0 {
		return fmt.Errorf("parallel upload needs a part size that is a multiple of 64 bytes, got %d", partSize)
	}

	// 并发上传需要随机读取，分片哈希上下文也必须在本地先算出来
	tmpF, err := stream.CacheFullAndWriter(nil, nil)
	if err != nil {
		return err
	}

	fullHash, partInfos, err := planParallelParts(tmpF, size, partSize)
	if err != nil {
		return err
	}

	// 筛选出前 100 个 partInfos
	firstPartInfos := partInfos
	if len(firstPartInfos) > 100 {
		firstPartInfos = firstPartInfos[:100]
	}

	// 创建任务，获取上传信息和前100个分片的上传地址
	data := base.Json{
		"contentHash":          fullHash,
		"contentHashAlgorithm": "SHA256",
		"contentType":          "application/octet-stream",
		"parallelUpload":       true,
		"partInfos":            firstPartInfos,
		"size":                 size,
		"parentFileId":         dstDir.GetID(),
		"name":                 stream.GetName(),
		"type":                 "file",
		"fileRenameMode":       "auto_rename",
	}
	var resp PersonalUploadResp
	if _, err = d.personalPost("/file/create", data, &resp); err != nil {
		return err
	}

	// 已存在同名同校验的文件，云端不会重复增加
	if resp.Data.Exist {
		return nil
	}

	// 没有返回分片上传地址即命中快传
	if resp.Data.PartInfos != nil {
		uploadPartInfos := resp.Data.PartInfos

		// 获取后续分片的上传地址
		for i := 100; i < len(partInfos); i += 100 {
			end := i + 100
			if end > len(partInfos) {
				end = len(partInfos)
			}
			moredata := base.Json{
				"fileId":    resp.Data.FileId,
				"uploadId":  resp.Data.UploadId,
				"partInfos": partInfos[i:end],
				"commonAccountInfo": base.Json{
					"account":     d.getAccount(),
					"accountType": 1,
				},
			}
			var moreresp PersonalUploadUrlResp
			if _, err = d.personalPost("/file/getUploadUrl", moredata, &moreresp); err != nil {
				return err
			}
			uploadPartInfos = append(uploadPartInfos, moreresp.Data.PartInfos...)
		}

		uploadThread := d.UploadThread
		if uploadThread <= 0 {
			uploadThread = 3
		}
		if uploadThread > len(uploadPartInfos) {
			uploadThread = len(uploadPartInfos)
		}

		progress := &parallelProgress{total: size, up: up}
		threadG, uploadCtx := errgroup.WithContext(ctx)
		threadG.SetLimit(uploadThread)
		for _, uploadPartInfo := range uploadPartInfos {
			if utils.IsCanceled(uploadCtx) {
				break
			}
			threadG.Go(func() error {
				index := uploadPartInfo.PartNumber - 1
				if index < 0 || index >= len(partInfos) {
					return fmt.Errorf("server returned unknown part number %d", uploadPartInfo.PartNumber)
				}
				part := partInfos[index]
				log.Debugf("[139] uploading part %+v/%+v", index, len(uploadPartInfos))

				reader := &parallelProgressReader{
					Reader:   io.NewSectionReader(tmpF, part.offset, part.PartSize),
					progress: progress,
				}
				req, err := http.NewRequestWithContext(uploadCtx, http.MethodPut, uploadPartInfo.UploadUrl,
					driver.NewLimitedUploadStream(uploadCtx, reader))
				if err != nil {
					return err
				}
				req.Header.Set("Content-Type", "application/octet-stream")
				req.Header.Set("Origin", "https://yun.139.com")
				req.Header.Set("Referer", "https://yun.139.com/")
				req.ContentLength = part.PartSize

				res, err := base.HttpClient.Do(req)
				if err != nil {
					return err
				}
				defer res.Body.Close()
				_, _ = io.Copy(io.Discard, res.Body)
				if res.StatusCode != http.StatusOK {
					return fmt.Errorf("part %d: unexpected status code: %d", uploadPartInfo.PartNumber, res.StatusCode)
				}
				return nil
			})
		}
		if err = threadG.Wait(); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}

		data = base.Json{
			"contentHash":          fullHash,
			"contentHashAlgorithm": "SHA256",
			"fileId":               resp.Data.FileId,
			"uploadId":             resp.Data.UploadId,
		}
		if _, err = d.personalPost("/file/complete", data, nil); err != nil {
			return err
		}
	}

	return d.resolveUploadRename(ctx, dstDir, stream, resp.Data.FileName)
}

// resolveUploadRename 处理 auto_rename 导致的重名：删除旧文件，再把新文件改回
// 原名。与串行路径中的冲突处理保持一致。
func (d *Yun139) resolveUploadRename(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, serverFileName string) error {
	if serverFileName == stream.GetName() {
		return nil
	}
	log.Debugf("[139] conflict detected: %s != %s", serverFileName, stream.GetName())
	// 给服务器一定时间处理数据，避免无法刷新文件列表
	time.Sleep(time.Millisecond * 500)
	files, err := d.List(ctx, dstDir, model.ListArgs{Refresh: true})
	if err != nil {
		return err
	}
	// 删除旧文件
	for _, file := range files {
		if file.GetName() == stream.GetName() {
			log.Debugf("[139] conflict: removing old: %s", file.GetName())
			// 删除前重命名旧文件，避免仍旧冲突
			if err = d.Rename(ctx, file, stream.GetName()+random.String(4)); err != nil {
				return err
			}
			if err = d.Remove(ctx, file); err != nil {
				return err
			}
			break
		}
	}
	// 重命名新文件
	for _, file := range files {
		if file.GetName() == serverFileName {
			log.Debugf("[139] conflict: renaming new: %s => %s", file.GetName(), stream.GetName())
			if err = d.Rename(ctx, file, stream.GetName()); err != nil {
				return err
			}
			break
		}
	}
	return nil
}
