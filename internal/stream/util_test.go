package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

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

func sliceForRange(data []byte, httpRange http_range.Range) []byte {
	start := int(httpRange.Start)
	length := int(httpRange.Length)
	if httpRange.Length < 0 || httpRange.Start+httpRange.Length > int64(len(data)) {
		length = len(data) - start
	}
	return data[start : start+length]
}
