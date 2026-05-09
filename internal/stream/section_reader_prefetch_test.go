package stream

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// signalReader is an io.Reader that records the high-water mark of bytes
// read so tests can deterministically observe how much of the source has
// been consumed at a given moment. It also exposes an optional per-Read
// delay so the test can create a window in which prefetch can run.
type signalReader struct {
	data     []byte
	readPos  atomic.Int64
	delay    time.Duration
	readCnt  atomic.Int32
	chunkSig chan int64 // optional: receives high-water mark after each Read
}

func newSignalReader(data []byte, delay time.Duration) *signalReader {
	return &signalReader{data: data, delay: delay, chunkSig: make(chan int64, 1024)}
}

func (r *signalReader) Read(p []byte) (int, error) {
	pos := r.readPos.Load()
	if pos >= int64(len(r.data)) {
		return 0, io.EOF
	}
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	n := copy(p, r.data[pos:])
	newPos := pos + int64(n)
	r.readPos.Store(newPos)
	r.readCnt.Add(1)
	select {
	case r.chunkSig <- newPos:
	default:
	}
	return n, nil
}

// fakeFileStream wraps a signalReader into a FileStreamer-compatible value
// reusing the production FileStream type.
func newFakeFileStream(t *testing.T, data []byte, delay time.Duration) (*FileStream, *signalReader) {
	t.Helper()
	sr := newSignalReader(data, delay)
	fs := &FileStream{
		Ctx:    context.Background(),
		Obj:    &model.Object{Name: "test.bin", Size: int64(len(data))},
		Reader: io.NopCloser(sr),
	}
	return fs, sr
}

// withStreamConf sets minimal stream/conf values needed for the
// hybridSectionReader path and restores them afterwards.
func withStreamConf(t *testing.T, cacheThreshold, maxBlock uint64) {
	t.Helper()
	prevCT := conf.AutoMemoryLimit
	prevMB := conf.MaxBlockLimit
	prevMF := conf.MinFreeMemory
	prevConf := conf.Conf
	t.Cleanup(func() {
		conf.AutoMemoryLimit = prevCT
		conf.MaxBlockLimit = prevMB
		conf.MinFreeMemory = prevMF
		conf.Conf = prevConf
	})
	conf.AutoMemoryLimit = cacheThreshold
	conf.MaxBlockLimit = maxBlock
	conf.MinFreeMemory = 1 // keep memory path enabled
	conf.Conf = &conf.Config{}
}

// TestHybridSectionReader_PrefetchAdvancesSourceAhead verifies that after
// GetSectionReader returns block N, the underlying source has been read
// past the end of block N (i.e., prefetch of block N+1 is in flight or
// already complete). This is the core behavior of Pass 2 prefetch.
func TestHybridSectionReader_PrefetchAdvancesSourceAhead(t *testing.T) {
	withStreamConf(t, 1024, 4*1024)

	const partSize = int64(4 * 1024)
	data := make([]byte, partSize*4)
	_, _ = rand.Read(data)

	// Per-Read delay so each chunk takes measurable time; this gives
	// prefetch a window to advance during the "upload" phase below.
	fs, sr := newFakeFileStream(t, data, 5*time.Millisecond)

	ss, err := NewStreamSectionReader(fs, int(partSize), nil)
	if err != nil {
		t.Fatalf("NewStreamSectionReader: %v", err)
	}
	hsr, ok := ss.(*hybridSectionReader)
	if !ok {
		t.Fatalf("expected *hybridSectionReader, got %T", ss)
	}
	_ = hsr

	// Read block 0
	rs, err := ss.GetSectionReader(0, partSize)
	if err != nil {
		t.Fatalf("GetSectionReader(0): %v", err)
	}
	got, err := io.ReadAll(rs)
	if err != nil {
		t.Fatalf("ReadAll block0: %v", err)
	}
	if !bytes.Equal(got, data[:partSize]) {
		t.Fatalf("block0 data mismatch")
	}

	// Simulate "upload" time; prefetch should pull block 1 from source.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sr.readPos.Load() > partSize {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	if got := sr.readPos.Load(); got <= partSize {
		t.Fatalf("expected prefetch to advance source past %d, got %d", partSize, got)
	}

	ss.FreeSectionReader(rs)

	// Read block 1 — should be served from prefetch (data must still be correct).
	rs2, err := ss.GetSectionReader(partSize, partSize)
	if err != nil {
		t.Fatalf("GetSectionReader(1): %v", err)
	}
	got2, err := io.ReadAll(rs2)
	if err != nil {
		t.Fatalf("ReadAll block1: %v", err)
	}
	if !bytes.Equal(got2, data[partSize:2*partSize]) {
		t.Fatalf("block1 data mismatch")
	}
	ss.FreeSectionReader(rs2)
}

// TestHybridSectionReader_PrefetchSequentialCorrectness verifies that a
// full sequential walk through the file returns the exact original bytes
// when prefetch is enabled.
func TestHybridSectionReader_PrefetchSequentialCorrectness(t *testing.T) {
	withStreamConf(t, 1024, 4*1024)

	const partSize = int64(4 * 1024)
	const partCount = 6
	data := make([]byte, partSize*partCount)
	_, _ = rand.Read(data)

	fs, _ := newFakeFileStream(t, data, 0)
	ss, err := NewStreamSectionReader(fs, int(partSize), nil)
	if err != nil {
		t.Fatalf("NewStreamSectionReader: %v", err)
	}

	for i := int64(0); i < partCount; i++ {
		off := i * partSize
		rs, err := ss.GetSectionReader(off, partSize)
		if err != nil {
			t.Fatalf("GetSectionReader(%d): %v", i, err)
		}
		got, err := io.ReadAll(rs)
		if err != nil {
			t.Fatalf("ReadAll part %d: %v", i, err)
		}
		if !bytes.Equal(got, data[off:off+partSize]) {
			t.Fatalf("part %d data mismatch", i)
		}
		ss.FreeSectionReader(rs)
	}
}

// TestHybridSectionReader_PrefetchLastChunkPartial covers the final
// partial chunk: when the file isn't a multiple of partSize, the last
// GetSectionReader call asks for a smaller length than the previous
// (prefetched) call expected. The data returned must still be correct.
func TestHybridSectionReader_PrefetchLastChunkPartial(t *testing.T) {
	withStreamConf(t, 1024, 4*1024)

	const partSize = int64(4 * 1024)
	// 2.5 parts: last chunk is partSize/2
	data := make([]byte, partSize*2+partSize/2)
	_, _ = rand.Read(data)

	fs, _ := newFakeFileStream(t, data, 0)
	ss, err := NewStreamSectionReader(fs, int(partSize), nil)
	if err != nil {
		t.Fatalf("NewStreamSectionReader: %v", err)
	}

	offsets := []struct{ off, length int64 }{
		{0, partSize},
		{partSize, partSize},
		{partSize * 2, partSize / 2},
	}
	for i, p := range offsets {
		rs, err := ss.GetSectionReader(p.off, p.length)
		if err != nil {
			t.Fatalf("GetSectionReader(%d, %d): %v", p.off, p.length, err)
		}
		got, err := io.ReadAll(rs)
		if err != nil {
			t.Fatalf("ReadAll part %d: %v", i, err)
		}
		if !bytes.Equal(got, data[p.off:p.off+p.length]) {
			t.Fatalf("part %d data mismatch", i)
		}
		ss.FreeSectionReader(rs)
	}
}

// TestHybridSectionReader_PrefetchErrorSurfaces verifies that a read
// error encountered during prefetch is reported on the next
// GetSectionReader call (rather than silently ignored).
func TestHybridSectionReader_PrefetchErrorSurfaces(t *testing.T) {
	withStreamConf(t, 1024, 4*1024)

	const partSize = int64(4 * 1024)
	data := make([]byte, partSize*3)
	_, _ = rand.Read(data)

	// Wrap the signal reader so reads targeting the second chunk return
	// a hard error and zero bytes — simulating mid-chunk download failure.
	sr := newSignalReader(data, 0)
	fs := &FileStream{
		Ctx: context.Background(),
		Obj: &model.Object{Name: "test.bin", Size: int64(len(data))},
		Reader: io.NopCloser(readerFunc(func(p []byte) (int, error) {
			if sr.readPos.Load() >= partSize {
				return 0, io.ErrUnexpectedEOF
			}
			remaining := partSize - sr.readPos.Load()
			if int64(len(p)) > remaining {
				p = p[:remaining]
			}
			return sr.Read(p)
		})),
	}

	ss, err := NewStreamSectionReader(fs, int(partSize), nil)
	if err != nil {
		t.Fatalf("NewStreamSectionReader: %v", err)
	}

	rs, err := ss.GetSectionReader(0, partSize)
	if err != nil {
		t.Fatalf("GetSectionReader(0): %v", err)
	}
	_, _ = io.ReadAll(rs)
	ss.FreeSectionReader(rs)

	// Wait for prefetch to complete (and presumably fail).
	time.Sleep(20 * time.Millisecond)

	_, err = ss.GetSectionReader(partSize, partSize)
	if err == nil {
		t.Fatalf("expected error from prefetch failure, got nil")
	}
}

// TestHybridSectionReader_NoPrefetchAfterEOF makes sure we don't start a
// prefetch goroutine past the end of the file.
func TestHybridSectionReader_NoPrefetchAfterEOF(t *testing.T) {
	withStreamConf(t, 1024, 4*1024)

	const partSize = int64(4 * 1024)
	data := make([]byte, partSize*2)
	_, _ = rand.Read(data)

	fs, sr := newFakeFileStream(t, data, 0)
	ss, err := NewStreamSectionReader(fs, int(partSize), nil)
	if err != nil {
		t.Fatalf("NewStreamSectionReader: %v", err)
	}

	for i := int64(0); i < 2; i++ {
		rs, err := ss.GetSectionReader(i*partSize, partSize)
		if err != nil {
			t.Fatalf("GetSectionReader(%d): %v", i, err)
		}
		_, _ = io.ReadAll(rs)
		ss.FreeSectionReader(rs)
	}

	// Allow any (incorrect) prefetch to fire.
	time.Sleep(20 * time.Millisecond)

	// Source must not have been read past end of file.
	if got := sr.readPos.Load(); got != int64(len(data)) {
		t.Fatalf("source read pos = %d, want %d (no over-read past EOF)", got, len(data))
	}

	hsr := ss.(*hybridSectionReader)
	if hsr.prefetch != nil {
		t.Fatalf("expected no in-flight prefetch after final chunk")
	}
}

// TestHybridSectionReader_PrefetchClampedToFileSize ensures the prefetch
// length is clamped to the remaining bytes so the goroutine doesn't
// request more than the file holds.
func TestHybridSectionReader_PrefetchClampedToFileSize(t *testing.T) {
	withStreamConf(t, 1024, 4*1024)

	const partSize = int64(4 * 1024)
	// 1.5 parts so prefetch after block 0 must be clamped to partSize/2.
	data := make([]byte, partSize+partSize/2)
	_, _ = rand.Read(data)

	fs, _ := newFakeFileStream(t, data, 0)
	ss, err := NewStreamSectionReader(fs, int(partSize), nil)
	if err != nil {
		t.Fatalf("NewStreamSectionReader: %v", err)
	}

	rs, err := ss.GetSectionReader(0, partSize)
	if err != nil {
		t.Fatalf("GetSectionReader(0): %v", err)
	}
	_, _ = io.ReadAll(rs)
	ss.FreeSectionReader(rs)

	rs2, err := ss.GetSectionReader(partSize, partSize/2)
	if err != nil {
		t.Fatalf("GetSectionReader(1): %v", err)
	}
	got, err := io.ReadAll(rs2)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data[partSize:]) {
		t.Fatalf("partial-tail data mismatch")
	}
	ss.FreeSectionReader(rs2)
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TestHybridSectionReader_PrefetchHonorsDiscardSection verifies that
// DiscardSection invalidates any pending prefetch and that subsequent
// GetSectionReader calls at the new offset return correct data.
func TestHybridSectionReader_PrefetchHonorsDiscardSection(t *testing.T) {
	withStreamConf(t, 1024, 4*1024)

	const partSize = int64(4 * 1024)
	data := make([]byte, partSize*4)
	_, _ = rand.Read(data)

	fs, _ := newFakeFileStream(t, data, 0)
	ss, err := NewStreamSectionReader(fs, int(partSize), nil)
	if err != nil {
		t.Fatalf("NewStreamSectionReader: %v", err)
	}

	// Read block 0 (kicks off prefetch of block 1)
	rs, err := ss.GetSectionReader(0, partSize)
	if err != nil {
		t.Fatalf("GetSectionReader: %v", err)
	}
	_, _ = io.ReadAll(rs)
	ss.FreeSectionReader(rs)

	// Give prefetch a moment to complete
	time.Sleep(20 * time.Millisecond)

	// Caller decides to skip block 1 — calls DiscardSection
	if err := ss.DiscardSection(partSize, partSize); err != nil {
		t.Fatalf("DiscardSection: %v", err)
	}

	// Now read block 2 — must return correct data
	rs2, err := ss.GetSectionReader(2*partSize, partSize)
	if err != nil {
		t.Fatalf("GetSectionReader after discard: %v", err)
	}
	got, err := io.ReadAll(rs2)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data[2*partSize:3*partSize]) {
		t.Fatalf("block 2 data mismatch after discard")
	}
	ss.FreeSectionReader(rs2)
}
