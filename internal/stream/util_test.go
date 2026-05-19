package stream

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// ensureConfForHTTP makes sure conf.Conf is non-nil before any test reaches
// net.HttpClient — that helper deferences conf.Conf.TlsInsecureSkipVerify
// during a sync.Once init and panics on a nil pointer otherwise.
func ensureConfForHTTP() {
	if conf.Conf == nil {
		conf.Conf = &conf.Config{}
	}
}

func TestRefreshableRangeReader_ReconnectsAfterMidStreamReset(t *testing.T) {
	data := []byte("0123456789abcdef")
	var refreshes int
	var mu sync.Mutex
	var resumedRanges []http_range.Range

	initial := RangeReaderFunc(func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
		return newFlakyReadCloser(sliceForRange(data, httpRange), 5, errors.New("read tcp 127.0.0.1:443: read: connection reset by peer")), nil
	})
	resumed := RangeReaderFunc(func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
		mu.Lock()
		resumedRanges = append(resumedRanges, httpRange)
		mu.Unlock()
		return io.NopCloser(bytes.NewReader(sliceForRange(data, httpRange))), nil
	})

	link := &model.Link{RangeReader: initial}
	link.Refresher = func(ctx context.Context) (*model.Link, model.Obj, error) {
		refreshes++
		return &model.Link{RangeReader: resumed}, nil, nil
	}

	reader, err := NewRefreshableRangeReader(link, int64(len(data))).RangeRead(context.Background(), http_range.Range{Start: 0, Length: int64(len(data))})
	if err != nil {
		t.Fatalf("RangeRead() error = %v", err)
	}
	defer reader.Close()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("ReadAll() = %q, want %q", got, data)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(resumedRanges) != 1 {
		t.Fatalf("len(resumedRanges) = %d, want 1", len(resumedRanges))
	}
	if resumedRanges[0].Start != 5 {
		t.Fatalf("resumed range start = %d, want 5", resumedRanges[0].Start)
	}
	if resumedRanges[0].Length != int64(len(data)-5) {
		t.Fatalf("resumed range length = %d, want %d", resumedRanges[0].Length, len(data)-5)
	}
}

func TestRefreshableRangeReader_ReconnectsAfterMidStreamReset_UnboundedRange(t *testing.T) {
	data := []byte("0123456789abcdef")
	var refreshes int
	var mu sync.Mutex
	var resumedRanges []http_range.Range

	initial := RangeReaderFunc(func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
		return newFlakyReadCloser(sliceForRange(data, httpRange), 5, errors.New("read tcp 127.0.0.1:443: read: connection reset by peer")), nil
	})
	resumed := RangeReaderFunc(func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
		mu.Lock()
		resumedRanges = append(resumedRanges, httpRange)
		mu.Unlock()
		return io.NopCloser(bytes.NewReader(sliceForRange(data, httpRange))), nil
	})

	link := &model.Link{RangeReader: initial}
	link.Refresher = func(ctx context.Context) (*model.Link, model.Obj, error) {
		refreshes++
		return &model.Link{RangeReader: resumed}, nil, nil
	}

	reader, err := NewRefreshableRangeReader(link, int64(len(data))).RangeRead(context.Background(), http_range.Range{Start: 0, Length: -1})
	if err != nil {
		t.Fatalf("RangeRead() error = %v", err)
	}
	defer reader.Close()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("ReadAll() = %q, want %q", got, data)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(resumedRanges) != 1 {
		t.Fatalf("len(resumedRanges) = %d, want 1", len(resumedRanges))
	}
	if resumedRanges[0].Start != 5 {
		t.Fatalf("resumed range start = %d, want 5", resumedRanges[0].Start)
	}
	if resumedRanges[0].Length != -1 {
		t.Fatalf("resumed range length = %d, want -1", resumedRanges[0].Length)
	}
}

// TestSelfHealingReadCloser_NormalEOFDoesNotTriggerReconnect verifies that a
// legitimate io.EOF (all data delivered) does NOT trigger a link refresh.
func TestSelfHealingReadCloser_NormalEOFDoesNotTriggerReconnect(t *testing.T) {
	data := []byte("hello world")
	refreshes := 0

	inner := RangeReaderFunc(func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(sliceForRange(data, httpRange))), nil
	})

	link := &model.Link{RangeReader: inner}
	link.Refresher = func(ctx context.Context) (*model.Link, model.Obj, error) {
		refreshes++
		return &model.Link{RangeReader: inner}, nil, nil
	}

	rrr := NewRefreshableRangeReader(link, int64(len(data)))
	rc, err := rrr.RangeRead(context.Background(), http_range.Range{Start: 0, Length: int64(len(data))})
	if err != nil {
		t.Fatalf("RangeRead error: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q, want %q", got, data)
	}
	if refreshes != 0 {
		t.Fatalf("refreshes = %d, want 0 (normal EOF should not trigger refresh)", refreshes)
	}
}

// TestSelfHealingReadCloser_UnexpectedEOFTriggersReconnect verifies that
// io.ErrUnexpectedEOF (stream interrupted) DOES trigger reconnect.
func TestSelfHealingReadCloser_UnexpectedEOFTriggersReconnect(t *testing.T) {
	data := []byte("0123456789")
	refreshes := 0

	initial := RangeReaderFunc(func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
		return newFlakyReadCloser(sliceForRange(data, httpRange), 4, io.ErrUnexpectedEOF), nil
	})
	resumed := RangeReaderFunc(func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(sliceForRange(data, httpRange))), nil
	})

	link := &model.Link{RangeReader: initial}
	link.Refresher = func(ctx context.Context) (*model.Link, model.Obj, error) {
		refreshes++
		return &model.Link{RangeReader: resumed}, nil, nil
	}

	rrr := NewRefreshableRangeReader(link, int64(len(data)))
	rc, err := rrr.RangeRead(context.Background(), http_range.Range{Start: 0, Length: int64(len(data))})
	if err != nil {
		t.Fatalf("RangeRead error: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q, want %q", got, data)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
}

// TestStreamHashFile_SeekablePrefetchProducesSameHash verifies that
// the prefetch optimization in StreamHashFile produces the exact same
// hash as a sequential read.
func TestStreamHashFile_SeekablePrefetchProducesSameHash(t *testing.T) {
	// 50 bytes = will be split into 10MB chunks in real code, but we
	// override chunkSize for testing. The key point: hash must be identical.
	data := []byte("The quick brown fox jumps over the lazy dog!!!!!") // 48 bytes

	rr := RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(sliceForRange(data, r))), nil
	})

	seekable := &SeekableStream{
		FileStream: &FileStream{
			Obj: &model.Object{Name: "test.bin", Size: int64(len(data))},
			Ctx: context.Background(),
		},
		rangeReader: rr,
	}

	hash1, err := StreamHashFile(seekable, utils.SHA1, 0, nil)
	if err != nil {
		t.Fatalf("StreamHashFile error: %v", err)
	}

	// Compute expected hash directly
	h := utils.SHA1.NewFunc()
	h.Write(data)
	expected := hex.EncodeToString(h.Sum(nil))

	if hash1 != expected {
		t.Fatalf("hash mismatch: got %s, want %s", hash1, expected)
	}
}

type flakyReadCloser struct {
	data      []byte
	failAfter int
	failErr   error
	failed    bool
}

func newFlakyReadCloser(data []byte, failAfter int, failErr error) *flakyReadCloser {
	return &flakyReadCloser{
		data:      data,
		failAfter: failAfter,
		failErr:   failErr,
	}
}

func (f *flakyReadCloser) Read(p []byte) (int, error) {
	if f.failed {
		return 0, io.EOF
	}
	if f.failAfter >= len(f.data) {
		f.failed = true
		n := copy(p, f.data)
		return n, io.EOF
	}

	n := copy(p, f.data[:f.failAfter])
	f.failed = true
	return n, f.failErr
}

func (f *flakyReadCloser) Close() error {
	return nil
}

// TestRangeReaderFromLink_SoftExpiredLink_TriggersRefresh covers the
// 115 CDN "soft 200 + empty body" failure mode: when a signed URL has
// expired, 115 still answers with HTTP 200 and Content-Length: 0 instead
// of a 4xx. Without explicit detection, OP forwards an empty stream to
// the client (mpv sees "Failed to recognize file format"). This test
// pins the contract that GetRangeReaderFromLink-derived readers treat
// "non-zero range requested → zero bytes promised" as an expired link
// and let RefreshableRangeReader trigger a refresh + retry.
func TestRangeReaderFromLink_SoftExpiredLink_TriggersRefresh(t *testing.T) {
	ensureConfForHTTP()
	data := []byte("The quick brown fox jumps over the lazy dog")
	size := int64(len(data))

	var expiredHits int32
	expired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&expiredHits, 1)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer expired.Close()

	var freshHits int32
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&freshHits, 1)
		rangeHeader := r.Header.Get("Range")
		start := int64(0)
		length := size
		if rangeHeader != "" {
			ranges, err := http_range.ParseRange(rangeHeader, size)
			if err == nil && len(ranges) == 1 {
				start = ranges[0].Start
				length = ranges[0].Length
				w.Header().Set("Content-Range", ranges[0].ContentRange(size))
				w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(data[start : start+length])
				return
			}
		}
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data[start : start+length])
	}))
	defer fresh.Close()

	link := &model.Link{URL: expired.URL}
	var refreshes int32
	link.Refresher = func(ctx context.Context) (*model.Link, model.Obj, error) {
		atomic.AddInt32(&refreshes, 1)
		return &model.Link{URL: fresh.URL}, nil, nil
	}

	rrr, err := GetRangeReaderFromLink(size, link)
	if err != nil {
		t.Fatalf("GetRangeReaderFromLink: %v", err)
	}
	rc, err := rrr.RangeRead(context.Background(), http_range.Range{Start: 0, Length: size})
	if err != nil {
		t.Fatalf("RangeRead: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("body mismatch: got %q, want %q", got, data)
	}
	if atomic.LoadInt32(&refreshes) != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	if atomic.LoadInt32(&expiredHits) < 1 {
		t.Fatalf("expired server never hit (= %d), expected at least once", expiredHits)
	}
	if atomic.LoadInt32(&freshHits) < 1 {
		t.Fatalf("fresh server never hit (= %d), expected at least once after refresh", freshHits)
	}
}

// TestRangeReaderFromLink_NormalResponse_NoFalsePositive guards against the
// soft-expired check ever firing on a healthy 206 response.
func TestRangeReaderFromLink_NormalResponse_NoFalsePositive(t *testing.T) {
	ensureConfForHTTP()
	data := []byte("0123456789abcdef")
	size := int64(len(data))

	var refreshes int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges, _ := http_range.ParseRange(r.Header.Get("Range"), size)
		if len(ranges) == 1 {
			ra := ranges[0]
			w.Header().Set("Content-Range", ra.ContentRange(size))
			w.Header().Set("Content-Length", strconv.FormatInt(ra.Length, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[ra.Start : ra.Start+ra.Length])
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		_, _ = w.Write(data)
	}))
	defer server.Close()

	link := &model.Link{URL: server.URL}
	link.Refresher = func(ctx context.Context) (*model.Link, model.Obj, error) {
		atomic.AddInt32(&refreshes, 1)
		return link, nil, nil
	}

	rrr, err := GetRangeReaderFromLink(size, link)
	if err != nil {
		t.Fatalf("GetRangeReaderFromLink: %v", err)
	}
	rc, err := rrr.RangeRead(context.Background(), http_range.Range{Start: 4, Length: 6})
	if err != nil {
		t.Fatalf("RangeRead: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data[4:10]) {
		t.Fatalf("body mismatch: got %q, want %q", got, data[4:10])
	}
	if atomic.LoadInt32(&refreshes) != 0 {
		t.Fatalf("refreshes = %d, want 0 (healthy response must not trigger refresh)", refreshes)
	}
}

// TestRangeReaderFromLink_ChunkedResponse_NoFalsePositive guards against
// false positives on responses without a Content-Length (chunked transfer).
// Go's http.Response sets ContentLength = -1 for chunked, which must not
// be treated as "0 bytes promised".
func TestRangeReaderFromLink_ChunkedResponse_NoFalsePositive(t *testing.T) {
	ensureConfForHTTP()
	data := []byte("chunked-payload-bytes-here-yo!")
	size := int64(len(data))

	var refreshes int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Force chunked: do not set Content-Length, write headers then body
		// in two flushes.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write(data[:len(data)/2])
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write(data[len(data)/2:])
	}))
	defer server.Close()

	link := &model.Link{URL: server.URL}
	link.Refresher = func(ctx context.Context) (*model.Link, model.Obj, error) {
		atomic.AddInt32(&refreshes, 1)
		return link, nil, nil
	}

	rrr, err := GetRangeReaderFromLink(size, link)
	if err != nil {
		t.Fatalf("GetRangeReaderFromLink: %v", err)
	}
	rc, err := rrr.RangeRead(context.Background(), http_range.Range{Start: 0, Length: size})
	if err != nil {
		t.Fatalf("RangeRead: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("body mismatch: got %q, want %q", got, data)
	}
	if atomic.LoadInt32(&refreshes) != 0 {
		t.Fatalf("refreshes = %d, want 0 (chunked response must not be treated as expired)", refreshes)
	}
}

// TestRangeReaderFromLink_SoftExpiredLink_NoRefresher_ReturnsError verifies
// that when a soft-expired link is encountered without a Refresher set,
// the error surfaces cleanly to the caller instead of returning an empty
// body that the client then misinterprets as a corrupt file.
func TestRangeReaderFromLink_SoftExpiredLink_NoRefresher_ReturnsError(t *testing.T) {
	ensureConfForHTTP()
	size := int64(100)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	link := &model.Link{URL: server.URL} // no Refresher

	rrr, err := GetRangeReaderFromLink(size, link)
	if err != nil {
		t.Fatalf("GetRangeReaderFromLink: %v", err)
	}
	_, err = rrr.RangeRead(context.Background(), http_range.Range{Start: 0, Length: size})
	if err == nil {
		t.Fatalf("RangeRead returned nil error; expected an 'expired link' error so callers see a real failure instead of an empty stream")
	}
	if !IsLinkExpiredError(err) {
		t.Fatalf("error %q is not classified as expired by IsLinkExpiredError", err)
	}
}

func sliceForRange(data []byte, httpRange http_range.Range) []byte {
	start := int(httpRange.Start)
	length := int(httpRange.Length)
	if httpRange.Length < 0 || httpRange.Start+httpRange.Length > int64(len(data)) {
		length = len(data) - start
	}
	return data[start : start+length]
}
