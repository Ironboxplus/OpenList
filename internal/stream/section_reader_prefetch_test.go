package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

func TestDirectSectionReader_SeekablePrefetchPipeline(t *testing.T) {
	data := []byte("abcdefgh")

	var (
		mu    sync.Mutex
		calls []http_range.Range
	)
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})

	rr := RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
		mu.Lock()
		calls = append(calls, r)
		mu.Unlock()

		if r.Start == 4 {
			select {
			case <-secondStarted:
			default:
				close(secondStarted)
			}
			<-releaseSecond
		}

		return io.NopCloser(bytes.NewReader(sliceForRange(data, r))), nil
	})

	seekable := newSeekableStreamForSectionTest(t, context.Background(), data, rr)
	ss, err := NewStreamSectionReader(seekable, 4, nil)
	if err != nil {
		t.Fatalf("NewStreamSectionReader() error = %v", err)
	}

	rd1, err := ss.GetSectionReader(0, 4)
	if err != nil {
		t.Fatalf("GetSectionReader(0,4) error = %v", err)
	}
	b1, err := io.ReadAll(rd1)
	if err != nil {
		t.Fatalf("ReadAll first chunk error = %v", err)
	}
	if !bytes.Equal(b1, data[0:4]) {
		t.Fatalf("first chunk = %q, want %q", b1, data[0:4])
	}
	ss.FreeSectionReader(rd1)

	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatalf("prefetch for second chunk did not start asynchronously")
	}

	type secondResult struct {
		data []byte
		err  error
	}
	resCh := make(chan secondResult, 1)
	go func() {
		rd2, err := ss.GetSectionReader(4, 4)
		if err != nil {
			resCh <- secondResult{err: err}
			return
		}
		b2, err := io.ReadAll(rd2)
		ss.FreeSectionReader(rd2)
		resCh <- secondResult{data: b2, err: err}
	}()

	select {
	case res := <-resCh:
		t.Fatalf("second GetSectionReader returned too early: err=%v data=%q", res.err, res.data)
	case <-time.After(100 * time.Millisecond):
		// expected: wait prefetch completion
	}

	close(releaseSecond)
	res := <-resCh
	if res.err != nil {
		t.Fatalf("GetSectionReader(4,4) error = %v", res.err)
	}
	if !bytes.Equal(res.data, data[4:8]) {
		t.Fatalf("second chunk = %q, want %q", res.data, data[4:8])
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("RangeRead call count = %d, want 2", len(calls))
	}
	if calls[0].Start != 0 || calls[1].Start != 4 {
		t.Fatalf("RangeRead starts = [%d, %d], want [0, 4]", calls[0].Start, calls[1].Start)
	}
}

func TestDirectSectionReader_SeekablePrefetchMissFallsBackToSyncRead(t *testing.T) {
	data := []byte("abcdefghijkl")

	var (
		mu    sync.Mutex
		calls []http_range.Range
	)
	prefetchStarted := make(chan struct{})

	rr := RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
		mu.Lock()
		calls = append(calls, r)
		mu.Unlock()

		if r.Start == 4 {
			select {
			case <-prefetchStarted:
			default:
				close(prefetchStarted)
			}
		}
		return io.NopCloser(bytes.NewReader(sliceForRange(data, r))), nil
	})

	seekable := newSeekableStreamForSectionTest(t, context.Background(), data, rr)
	ss, err := NewStreamSectionReader(seekable, 4, nil)
	if err != nil {
		t.Fatalf("NewStreamSectionReader() error = %v", err)
	}

	rd1, err := ss.GetSectionReader(0, 4)
	if err != nil {
		t.Fatalf("GetSectionReader(0,4) error = %v", err)
	}
	_, _ = io.ReadAll(rd1)
	ss.FreeSectionReader(rd1)

	select {
	case <-prefetchStarted:
	case <-time.After(2 * time.Second):
		t.Fatalf("prefetch for second chunk did not start")
	}

	rd3, err := ss.GetSectionReader(8, 4)
	if err != nil {
		t.Fatalf("GetSectionReader(8,4) error = %v", err)
	}
	b3, err := io.ReadAll(rd3)
	if err != nil {
		t.Fatalf("ReadAll third chunk error = %v", err)
	}
	ss.FreeSectionReader(rd3)
	if !bytes.Equal(b3, data[8:12]) {
		t.Fatalf("third chunk = %q, want %q", b3, data[8:12])
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("RangeRead call count = %d, want 3", len(calls))
	}
	starts := []int64{calls[0].Start, calls[1].Start, calls[2].Start}
	if !((starts[0] == 0 && starts[1] == 4 && starts[2] == 8) || (starts[0] == 0 && starts[1] == 8 && starts[2] == 4)) {
		t.Fatalf("unexpected RangeRead starts sequence: %v", starts)
	}
}

func TestDirectSectionReader_SeekablePrefetchDoesNotReportProgress(t *testing.T) {
	data := []byte("abcdefgh")

	var progressCalls int
	up := model.UpdateProgress(func(_ float64) {
		progressCalls++
	})

	rr := RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(sliceForRange(data, r))), nil
	})

	seekable := newSeekableStreamForSectionTest(t, context.Background(), data, rr)
	ss, err := NewStreamSectionReader(seekable, 4, &up)
	if err != nil {
		t.Fatalf("NewStreamSectionReader() error = %v", err)
	}

	rd1, err := ss.GetSectionReader(0, 4)
	if err != nil {
		t.Fatalf("GetSectionReader(0,4) error = %v", err)
	}
	_, _ = io.ReadAll(rd1)
	ss.FreeSectionReader(rd1)

	rd2, err := ss.GetSectionReader(4, 4)
	if err != nil {
		t.Fatalf("GetSectionReader(4,4) error = %v", err)
	}
	_, _ = io.ReadAll(rd2)
	ss.FreeSectionReader(rd2)

	if progressCalls != 0 {
		t.Fatalf("progress callback called %d times, want 0", progressCalls)
	}
}

func TestDirectSectionReader_SeekablePrefetchFailureIsObservable(t *testing.T) {
	data := []byte("abcdefgh")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	var onceStarted sync.Once

	rr := RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
		if r.Start == 4 {
			onceStarted.Do(func() { close(started) })
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return io.NopCloser(bytes.NewReader(sliceForRange(data, r))), nil
	})

	seekable := newSeekableStreamForSectionTest(t, ctx, data, rr)
	ss, err := NewStreamSectionReader(seekable, 4, nil)
	if err != nil {
		t.Fatalf("NewStreamSectionReader() error = %v", err)
	}

	rd1, err := ss.GetSectionReader(0, 4)
	if err != nil {
		t.Fatalf("GetSectionReader(0,4) error = %v", err)
	}
	_, _ = io.ReadAll(rd1)
	ss.FreeSectionReader(rd1)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatalf("prefetch did not start")
	}

	cancel()
	_, err = ss.GetSectionReader(4, 4)
	if err == nil {
		t.Fatalf("GetSectionReader(4,4) expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled error, got: %v", err)
	}
}

func TestDirectSectionReader_SeekablePrefetchStopsOnContextCancel(t *testing.T) {
	data := []byte("abcdefgh")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	exited := make(chan struct{})
	var onceStarted, onceExited sync.Once

	rr := RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
		if r.Start == 4 {
			onceStarted.Do(func() { close(started) })
			<-ctx.Done()
			onceExited.Do(func() { close(exited) })
		}
		return io.NopCloser(bytes.NewReader(sliceForRange(data, r))), nil
	})

	seekable := newSeekableStreamForSectionTest(t, ctx, data, rr)
	ss, err := NewStreamSectionReader(seekable, 4, nil)
	if err != nil {
		t.Fatalf("NewStreamSectionReader() error = %v", err)
	}

	rd1, err := ss.GetSectionReader(0, 4)
	if err != nil {
		t.Fatalf("GetSectionReader(0,4) error = %v", err)
	}
	_, _ = io.ReadAll(rd1)
	ss.FreeSectionReader(rd1)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatalf("prefetch did not start")
	}

	cancel()

	select {
	case <-exited:
		// pass: 预读协程在 ctx cancel 后退出
	case <-time.After(2 * time.Second):
		t.Fatalf("prefetch goroutine did not exit after context cancel")
	}
}

func newSeekableStreamForSectionTest(t *testing.T, ctx context.Context, data []byte, rr model.RangeReaderIF) *SeekableStream {
	t.Helper()
	obj := &model.Object{Name: "section-test.bin", Size: int64(len(data))}
	fs := &FileStream{Ctx: ctx, Obj: obj}
	link := &model.Link{RangeReader: rr, ContentLength: int64(len(data))}
	ss, err := NewSeekableStream(fs, link)
	if err != nil {
		t.Fatalf("NewSeekableStream() error = %v", err)
	}
	t.Cleanup(func() {
		_ = ss.Close()
	})
	return ss
}
