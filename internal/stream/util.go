package stream

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/net"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/pool"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/rclone/rclone/lib/mmap"
	log "github.com/sirupsen/logrus"
)

type RangeReaderFunc func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error)

func (f RangeReaderFunc) RangeRead(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
	return f(ctx, httpRange)
}

func GetRangeReaderFromLink(size int64, link *model.Link) (model.RangeReaderIF, error) {
	if link.RangeReader != nil {
		if link.Concurrency < 1 && link.PartSize < 1 {
			return link.RangeReader, nil
		}
		down := net.NewDownloader(func(d *net.Downloader) {
			d.Concurrency = link.Concurrency
			d.PartSize = link.PartSize
			d.HttpClient = net.GetRangeReaderHttpRequestFunc(link.RangeReader)
		})
		rangeReader := func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
			return down.Download(ctx, &net.HttpRequestParams{
				Range: httpRange,
				Size:  size,
			})
		}
		// RangeReader只能在驱动限速
		return RangeReaderFunc(rangeReader), nil
	}

	if len(link.URL) == 0 {
		return nil, errors.New("invalid link: must have at least one of URL or RangeReader")
	}

	if link.Concurrency > 0 || link.PartSize > 0 {
		down := net.NewDownloader(func(d *net.Downloader) {
			d.Concurrency = link.Concurrency
			d.PartSize = link.PartSize
			d.HttpClient = func(ctx context.Context, params *net.HttpRequestParams) (*http.Response, error) {
				if ServerDownloadLimit == nil {
					return net.DefaultHttpRequestFunc(ctx, params)
				}
				resp, err := net.DefaultHttpRequestFunc(ctx, params)
				if err == nil && resp.Body != nil {
					resp.Body = &RateLimitReader{
						Ctx:     ctx,
						Reader:  resp.Body,
						Limiter: ServerDownloadLimit,
					}
				}
				return resp, err
			}
		})
		rangeReader := func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
			requestHeader, _ := ctx.Value(conf.RequestHeaderKey).(http.Header)
			header := net.ProcessHeader(requestHeader, link.Header)
			return down.Download(ctx, &net.HttpRequestParams{
				Range:     httpRange,
				Size:      size,
				URL:       link.URL,
				HeaderRef: header,
			})
		}
		return RangeReaderFunc(rangeReader), nil
	}

	rangeReader := func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
		if httpRange.Length < 0 || httpRange.Start+httpRange.Length > size {
			httpRange.Length = size - httpRange.Start
		}
		requestHeader, _ := ctx.Value(conf.RequestHeaderKey).(http.Header)
		header := net.ProcessHeader(requestHeader, link.Header)
		header = http_range.ApplyRangeToHttpHeader(httpRange, header)

		response, err := net.RequestHttp(ctx, "GET", header, link.URL)
		if err != nil {
			if _, ok := errs.UnwrapOrSelf(err).(net.HttpStatusCodeError); ok {
				return nil, err
			}
			return nil, fmt.Errorf("http request failure, err:%w", err)
		}
		if ServerDownloadLimit != nil {
			response.Body = &RateLimitReader{
				Ctx:     ctx,
				Reader:  response.Body,
				Limiter: ServerDownloadLimit,
			}
		}
		if httpRange.Start == 0 && httpRange.Length == size ||
			response.StatusCode == http.StatusPartialContent ||
			checkContentRange(&response.Header, httpRange.Start) {
			return response.Body, nil
		} else if response.StatusCode == http.StatusOK {
			log.Warnf("remote http server not supporting range request, expect low perfromace!")
			readCloser, err := net.GetRangedHttpReader(response.Body, httpRange.Start, httpRange.Length)
			if err != nil {
				return nil, err
			}
			return readCloser, nil
		}
		return response.Body, nil
	}
	return RangeReaderFunc(rangeReader), nil
}

func GetRangeReaderFromMFile(size int64, file model.File) *model.FileRangeReader {
	return &model.FileRangeReader{
		RangeReaderIF: RangeReaderFunc(func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
			length := httpRange.Length
			if length < 0 || httpRange.Start+length > size {
				length = size - httpRange.Start
			}
			return &model.FileCloser{File: io.NewSectionReader(file, httpRange.Start, length)}, nil
		}),
	}
}

// 139 cloud does not properly return 206 http status code, add a hack here
func checkContentRange(header *http.Header, offset int64) bool {
	start, _, err := http_range.ParseContentRange(header.Get("Content-Range"))
	if err != nil {
		log.Warnf("exception trying to parse Content-Range, will ignore,err=%s", err)
	}
	if start == offset {
		return true
	}
	return false
}

type ReaderWithCtx struct {
	io.Reader
	Ctx context.Context
}

func (r *ReaderWithCtx) Read(p []byte) (n int, err error) {
	if utils.IsCanceled(r.Ctx) {
		return 0, r.Ctx.Err()
	}
	return r.Reader.Read(p)
}

func (r *ReaderWithCtx) Close() error {
	if c, ok := r.Reader.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func CacheFullAndHash(stream model.FileStreamer, up *model.UpdateProgress, hashType *utils.HashType, hashParams ...any) (model.File, string, error) {
	h := hashType.NewFunc(hashParams...)
	tmpF, err := stream.CacheFullAndWriter(up, h)
	if err != nil {
		return nil, "", err
	}
	return tmpF, hex.EncodeToString(h.Sum(nil)), nil
}

// readFullWithRangeRead 使用 RangeRead 从文件流中读取数据到 buf
// file: 文件流
// buf: 目标缓冲区
// off: 读取的起始偏移量
// 返回值: 实际读取的字节数和错误
// 支持自动重试（最多3次），每次重试之间有递增延迟（3秒、6秒、9秒）
func readFullWithRangeRead(file model.FileStreamer, buf []byte, off int64) (int, error) {
	length := int64(len(buf))
	var lastErr error

	// 重试最多3次
	for retry := 0; retry < 3; retry++ {
		reader, err := file.RangeRead(http_range.Range{Start: off, Length: length})
		if err != nil {
			lastErr = fmt.Errorf("RangeRead failed at offset %d: %w", off, err)
			log.Debugf("RangeRead retry %d failed: %v", retry+1, lastErr)
			// 递增延迟：3秒、6秒、9秒，等待代理恢复
			time.Sleep(time.Duration(retry+1) * 3 * time.Second)
			continue
		}

		n, err := io.ReadFull(reader, buf)
		if closer, ok := reader.(io.Closer); ok {
			closer.Close()
		}

		if err == nil {
			return n, nil
		}

		lastErr = fmt.Errorf("failed to read all data via RangeRead at offset %d: (expect=%d, actual=%d) %w", off, length, n, err)
		log.Debugf("RangeRead retry %d read failed: %v", retry+1, lastErr)
		// 递增延迟：3秒、6秒、9秒，等待网络恢复
		time.Sleep(time.Duration(retry+1) * 3 * time.Second)
	}

	return 0, lastErr
}

// StreamHashFile 流式计算文件哈希值，避免将整个文件加载到内存
// file: 文件流
// hashType: 哈希算法类型
// progressWeight: 进度权重（0-100），用于计算整体进度
// up: 进度回调函数
func StreamHashFile(file model.FileStreamer, hashType *utils.HashType, progressWeight float64, up *model.UpdateProgress) (string, error) {
	// 如果已经有完整缓存文件，直接使用
	if cache := file.GetFile(); cache != nil {
		hashFunc := hashType.NewFunc()
		cache.Seek(0, io.SeekStart)
		_, err := io.Copy(hashFunc, cache)
		if err != nil {
			return "", err
		}
		if up != nil && progressWeight > 0 {
			(*up)(progressWeight)
		}
		return hex.EncodeToString(hashFunc.Sum(nil)), nil
	}

	hashFunc := hashType.NewFunc()
	size := file.GetSize()
	chunkSize := int64(10 * 1024 * 1024) // 10MB per chunk
	buf := make([]byte, chunkSize)
	var offset int64 = 0

	for offset < size {
		readSize := chunkSize
		if size-offset < chunkSize {
			readSize = size - offset
		}

		var n int
		var err error

		// 对于 SeekableStream，优先使用 RangeRead 避免消耗 Reader
		// 这样后续发送时 Reader 还能正常工作
		if _, ok := file.(*SeekableStream); ok {
			n, err = readFullWithRangeRead(file, buf[:readSize], offset)
		} else {
			// 对于 FileStream，首先尝试顺序流读取（不消耗额外资源，适用于所有流类型）
			n, err = io.ReadFull(file, buf[:readSize])
			if err != nil {
				// 顺序流读取失败，尝试使用 RangeRead 重试（适用于 SeekableStream）
				log.Warnf("StreamHashFile: sequential read failed at offset %d, retrying with RangeRead: %v", offset, err)
				n, err = readFullWithRangeRead(file, buf[:readSize], offset)
			}
		}

		if err != nil {
			return "", fmt.Errorf("calculate hash failed at offset %d: %w", offset, err)
		}

		hashFunc.Write(buf[:n])
		offset += int64(n)

		if up != nil && progressWeight > 0 {
			progress := progressWeight * float64(offset) / float64(size)
			(*up)(progress)
		}
	}

	return hex.EncodeToString(hashFunc.Sum(nil)), nil
}

type StreamSectionReaderIF interface {
	// 线程不安全
	GetSectionReader(off, length int64) (io.ReadSeeker, error)
	FreeSectionReader(sr io.ReadSeeker)
	// 线程不安全
	DiscardSection(off int64, length int64) error
}

func NewStreamSectionReader(file model.FileStreamer, maxBufferSize int, up *model.UpdateProgress) (StreamSectionReaderIF, error) {
	if file.GetFile() != nil {
		return &cachedSectionReader{file.GetFile()}, nil
	}

	maxBufferSize = min(maxBufferSize, int(file.GetSize()))

	// 始终使用 directSectionReader，只在内存中缓存当前分片
	// 避免创建临时文件导致中间文件增长到整个文件大小
	ss := &directSectionReader{file: file}
	if conf.MmapThreshold > 0 && maxBufferSize >= conf.MmapThreshold {
		ss.bufPool = &pool.Pool[[]byte]{
			New: func() []byte {
				buf, err := mmap.Alloc(maxBufferSize)
				if err == nil {
					file.Add(utils.CloseFunc(func() error {
						return mmap.Free(buf)
					}))
				} else {
					buf = make([]byte, maxBufferSize)
				}
				return buf
			},
		}
	} else {
		ss.bufPool = &pool.Pool[[]byte]{
			New: func() []byte {
				return make([]byte, maxBufferSize)
			},
		}
	}

	file.Add(utils.CloseFunc(func() error {
		ss.bufPool.Reset()
		return nil
	}))
	return ss, nil
}

type cachedSectionReader struct {
	cache io.ReaderAt
}

func (*cachedSectionReader) DiscardSection(off int64, length int64) error {
	return nil
}
func (s *cachedSectionReader) GetSectionReader(off, length int64) (io.ReadSeeker, error) {
	return io.NewSectionReader(s.cache, off, length), nil
}
func (*cachedSectionReader) FreeSectionReader(sr io.ReadSeeker) {}

type fileSectionReader struct {
	file       model.FileStreamer
	fileOffset int64
	temp       *os.File
	tempOffset int64
	bufPool    *pool.Pool[*offsetWriterWithBase]
}

type offsetWriterWithBase struct {
	*io.OffsetWriter
	base int64
}

// 线程不安全
func (ss *fileSectionReader) DiscardSection(off int64, length int64) error {
	if off != ss.fileOffset {
		return fmt.Errorf("stream not cached: request offset %d != current offset %d", off, ss.fileOffset)
	}
	n, err := utils.CopyWithBufferN(io.Discard, ss.file, length)
	ss.fileOffset += n
	if err != nil {
		return fmt.Errorf("failed to skip data: (expect =%d, actual =%d) %w", length, n, err)
	}
	return nil
}

type fileBufferSectionReader struct {
	io.ReadSeeker
	fileBuf *offsetWriterWithBase
}

// 线程不安全
func (ss *fileSectionReader) GetSectionReader(off, length int64) (io.ReadSeeker, error) {
	if off != ss.fileOffset {
		return nil, fmt.Errorf("stream not cached: request offset %d != current offset %d", off, ss.fileOffset)
	}
	fileBuf := ss.bufPool.Get()
	_, _ = fileBuf.Seek(0, io.SeekStart)
	n, err := utils.CopyWithBufferN(fileBuf, ss.file, length)
	ss.fileOffset += n
	if err != nil {
		return nil, fmt.Errorf("failed to read all data: (expect =%d, actual =%d) %w", length, n, err)
	}
	return &fileBufferSectionReader{io.NewSectionReader(ss.temp, fileBuf.base, length), fileBuf}, nil
}

func (ss *fileSectionReader) FreeSectionReader(rs io.ReadSeeker) {
	if sr, ok := rs.(*fileBufferSectionReader); ok {
		ss.bufPool.Put(sr.fileBuf)
		sr.fileBuf = nil
		sr.ReadSeeker = nil
	}
}

type directSectionReader struct {
	file       model.FileStreamer
	fileOffset int64
	bufPool    *pool.Pool[[]byte]
}

// 线程不安全（依赖调用方保证串行调用）
// 对于 SeekableStream：直接跳过（无需实际读取）
// 对于 FileStream：必须顺序读取并丢弃
func (ss *directSectionReader) DiscardSection(off int64, length int64) error {
	// 对于 SeekableStream，直接跳过（RangeRead 支持随机访问，不需要实际读取）
	if _, ok := ss.file.(*SeekableStream); ok {
		return nil
	}

	// 对于 FileStream，必须顺序读取并丢弃
	if off != ss.fileOffset {
		return fmt.Errorf("stream not cached: request offset %d != current offset %d", off, ss.fileOffset)
	}
	n, err := utils.CopyWithBufferN(io.Discard, ss.file, length)
	ss.fileOffset += n
	if err != nil {
		return fmt.Errorf("failed to skip data: (expect =%d, actual =%d) %w", length, n, err)
	}
	return nil
}

type bufferSectionReader struct {
	io.ReadSeeker
	buf []byte
}

// 线程不安全（依赖调用方保证串行调用）
// 对于 SeekableStream：使用 RangeRead，支持随机访问（续传场景可跳过已上传分片）
// 对于 FileStream：必须顺序读取
func (ss *directSectionReader) GetSectionReader(off, length int64) (io.ReadSeeker, error) {
	tempBuf := ss.bufPool.Get()
	buf := tempBuf[:length]

	// 对于 SeekableStream，直接使用 RangeRead（支持随机访问，适用于续传场景）
	if _, ok := ss.file.(*SeekableStream); ok {
		n, err := readFullWithRangeRead(ss.file, buf, off)
		if err != nil {
			ss.bufPool.Put(tempBuf)
			return nil, fmt.Errorf("RangeRead failed at offset %d: (expect=%d, actual=%d) %w", off, length, n, err)
		}
		return &bufferSectionReader{bytes.NewReader(buf), tempBuf}, nil
	}

	// 对于 FileStream，必须顺序读取
	if off != ss.fileOffset {
		ss.bufPool.Put(tempBuf)
		return nil, fmt.Errorf("stream not cached: request offset %d != current offset %d", off, ss.fileOffset)
	}

	n, err := io.ReadFull(ss.file, buf)
	if err != nil {
		ss.bufPool.Put(tempBuf)
		return nil, fmt.Errorf("sequential read failed at offset %d: (expect=%d, actual=%d) %w", off, length, n, err)
	}

	ss.fileOffset = off + int64(n)
	return &bufferSectionReader{bytes.NewReader(buf), tempBuf}, nil
}
func (ss *directSectionReader) FreeSectionReader(rs io.ReadSeeker) {
	if sr, ok := rs.(*bufferSectionReader); ok {
		ss.bufPool.Put(sr.buf[0:cap(sr.buf)])
		sr.buf = nil
		sr.ReadSeeker = nil
	}
}
