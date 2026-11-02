package net

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

// shrinkOverloadBackoff makes the retry pauses of an overloaded server short
// enough for a test.
func shrinkOverloadBackoff(t *testing.T) {
	t.Helper()
	oldUnit, oldMax := overloadBackoffUnit, overloadBackoffMax
	overloadBackoffUnit, overloadBackoffMax = time.Millisecond, 3*time.Millisecond
	t.Cleanup(func() { overloadBackoffUnit, overloadBackoffMax = oldUnit, oldMax })
}

// partFake serves data in ranges; other(start) decides what a request for a
// part after the first gets: a status error to return, or nil to serve it.
type partFake struct {
	data  []byte
	other func(start int64, attempt int) error
	mu    sync.Mutex
	tries map[int64]int
	total atomic.Int64
}

func (f *partFake) request(_ context.Context, p *HttpRequestParams) (*http.Response, error) {
	f.total.Add(1)
	start := p.Range.Start
	if start > 0 && f.other != nil {
		f.mu.Lock()
		if f.tries == nil {
			f.tries = map[int64]int{}
		}
		f.tries[start]++
		n := f.tries[start]
		f.mu.Unlock()
		if err := f.other(start, n); err != nil {
			return nil, err
		}
	}
	end := start + p.Range.Length
	if p.Range.Length < 0 || end > int64(len(f.data)) {
		end = int64(len(f.data))
	}
	h := http.Header{}
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(f.data)))
	return &http.Response{StatusCode: http.StatusPartialContent, Header: h, ContentLength: end - start,
		Body: io.NopCloser(bytes.NewReader(f.data[start:end]))}, nil
}

func (f *partFake) downloader(retries int) *Downloader {
	return NewDownloader(func(d *Downloader) {
		d.Concurrency = 3
		d.PartSize = 4
		d.PartBodyMaxRetries = retries
		d.HttpClient = f.request
	})
}

// readAll reads the whole download, failing the test (instead of hanging it)
// when that takes longer than 10s.
func readAll(t *testing.T, d *Downloader, size int64) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		rc, err := d.Download(ctx, &HttpRequestParams{Size: size, Range: http_range.Range{Length: -1}})
		if err != nil {
			done <- result{nil, err}
			return
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		done <- result{b, err}
	}()
	select {
	case r := <-done:
		return r.b, r.err
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("download did not finish within 10s")
		return nil, nil
	}
}

func testData() []byte {
	b := make([]byte, 40) // ten parts of 4 bytes
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	return b
}

// A: the first part is fine and every later one is refused with 403 (an expired
// link). That is not overload; the download must fail, naming 403.
// The old code treated every status of a later part as overload and retried it
// forever (errInfiniteRetry does retry--), so the reader never returned.
func TestLaterPartForbiddenFailsInsteadOfRetryingForever(t *testing.T) {
	shrinkOverloadBackoff(t)
	f := &partFake{data: testData(), other: func(int64, int) error { return HttpStatusCodeError(http.StatusForbidden) }}
	_, err := readAll(t, f.downloader(3), int64(len(f.data)))
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want one naming 403", err)
	}
}

// B: a later part answers 503 twice and then works: the content arrives whole.
// (The old code also got through this, only slowly; the test keeps the retry
// of real overload intact.)
func TestLaterPartOverloadedThenServedIsComplete(t *testing.T) {
	shrinkOverloadBackoff(t)
	f := &partFake{data: testData(), other: func(_ int64, attempt int) error {
		if attempt <= 2 {
			return HttpStatusCodeError(http.StatusServiceUnavailable)
		}
		return nil
	}}
	got, err := readAll(t, f.downloader(3), int64(len(f.data)))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if sha1.Sum(got) != sha1.Sum(f.data) {
		t.Fatalf("content differs: got %v want %v", got, f.data)
	}
}

// C: a later part that stays overloaded gives up after PartBodyMaxRetries*10
// overload retries, with an error, and does not keep asking.
// The old code never gave up.
func TestLaterPartAlwaysOverloadedGivesUp(t *testing.T) {
	shrinkOverloadBackoff(t)
	const retries = 2
	f := &partFake{data: testData(), other: func(int64, int) error { return HttpStatusCodeError(http.StatusServiceUnavailable) }}
	_, err := readAll(t, f.downloader(retries), int64(len(f.data)))
	if err == nil {
		t.Fatal("download of an always-overloaded server succeeded")
	}
	if !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("err = %v, want the give-up error", err)
	}
	// every worker (Concurrency=3) asks at most retries*10+1 times per part it
	// holds, plus the requests of the first part
	if max := int64(3*(retries*10+1) + 3); f.total.Load() > max {
		t.Fatalf("%d requests, want at most %d", f.total.Load(), max)
	}
}

func TestIsOverloadStatus(t *testing.T) {
	for code, want := range map[int]bool{429: true, 502: true, 503: true, 504: true, 200: false, 400: false, 401: false, 403: false, 404: false, 500: false} {
		if got := isOverloadStatus(code); got != want {
			t.Errorf("isOverloadStatus(%d) = %v, want %v", code, got, want)
		}
	}
}
