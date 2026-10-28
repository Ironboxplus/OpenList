package cfs_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/local"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// hashProbe is the Local driver that remembers the SHA-1 each stream it is
// given carried.
type hashProbe struct{ local.Local }

var probed sync.Map // file name -> SHA-1 of the stream the driver received

func (p *hashProbe) Config() driver.Config {
	c := p.Local.Config()
	c.Name = "HashProbe"
	return c
}

func (p *hashProbe) Put(ctx context.Context, dstDir model.Obj, s model.FileStreamer, up driver.UpdateProgress) error {
	probed.Store(s.GetName(), s.GetHash().GetHash(utils.SHA1))
	return p.Local.Put(ctx, dstDir, s, up)
}

func init() {
	op.RegisterDriver(func() driver.Driver { return &hashProbe{} })
}

// tempFiles samples the temp dir until stop is closed and reports the most
// cache files it held at once.
func tempFiles(dir string, stop <-chan struct{}) *atomic.Int64 {
	var peak atomic.Int64
	go func() {
		for {
			ents, _ := os.ReadDir(dir)
			var n int64
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), "file-") {
					n++
				}
			}
			if n > peak.Load() {
				peak.Store(n)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	return &peak
}

func withTempDir(t *testing.T) string {
	t.Helper()
	old, oldMem := conf.Conf.TempDir, conf.MinFreeMemory
	dir := t.TempDir()
	conf.Conf.TempDir = dir
	conf.MinFreeMemory = 0 // caches go to files, where they can be counted
	t.Cleanup(func() { conf.Conf.TempDir, conf.MinFreeMemory = old, oldMem })
	return dir
}

func newSeekable(t *testing.T, name string, data []byte, hash string) *stream.SeekableStream {
	t.Helper()
	obj := &model.Object{Name: name, Size: int64(len(data)), Modified: time.Now()}
	if hash != "" {
		obj.HashInfo = utils.NewHashInfo(utils.SHA1, hash)
	}
	ss, err := stream.NewSeekableStream(&stream.FileStream{Ctx: context.Background(), Obj: obj}, &model.Link{
		ContentLength: int64(len(data)),
		RangeReader: stream.RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
			end := int64(len(data))
			if r.Length >= 0 {
				end = min(end, r.Start+r.Length)
			}
			return io.NopCloser(bytes.NewReader(data[r.Start:end])), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

// plainStream is a stream that can only be read once, front to back.
func plainStream(name string, data []byte, size int64) *stream.FileStream {
	return &stream.FileStream{
		Ctx:    context.Background(),
		Obj:    &model.Object{Name: name, Size: size, Modified: time.Now()},
		Reader: struct{ io.Reader }{bytes.NewReader(data)},
	}
}

func hexSHA1(b []byte) string { s := sha1.Sum(b); return hex.EncodeToString(s[:]) }

// A big file whose stream can be read by range, or is already a file, is not
// cached again; one that can only be read once is cached exactly once; and one
// that would not fit leaves nothing behind.
func TestPutBigFileCaching(t *testing.T) {
	ctx := context.Background()
	temp := withTempDir(t)
	under, state := t.TempDir(), t.TempDir()
	addStorage(t, "Local", "/cu", `{"root_folder_path":`+q(under)+`}`)
	id := addStorage(t, "Cfs", "/cc", `{"remote_path":"/cu/vol","root_folder_path":"/","state_dir":`+q(state)+`,"seal_quiet":"50ms","seal_max":"500ms","threshold":"64KiB"}`)
	t.Cleanup(func() { op.DeleteStorageById(ctx, id) })
	st, err := op.GetStorageByMountPath("/cc")
	if err != nil {
		t.Fatal(err)
	}
	put := func(s model.FileStreamer) error { return op.Put(ctx, st, "/", s, func(float64) {}) }
	measure := func(f func()) int64 {
		stop := make(chan struct{})
		peak := tempFiles(temp, stop)
		f()
		time.Sleep(20 * time.Millisecond)
		close(stop)
		time.Sleep(5 * time.Millisecond)
		return peak.Load()
	}
	big := randBytes(6 << 20)

	// Read by range from its source: nothing is cached.
	n := measure(func() {
		if err := put(newSeekable(t, "ranged.bin", big, hexSHA1(big))); err != nil {
			t.Fatalf("put of a seekable stream: %v", err)
		}
	})
	t.Logf("seekable stream: %d cache files in the temp dir", n)
	if n != 0 {
		t.Fatalf("a seekable stream was cached (%d files)", n)
	}
	if got := readAll(t, "/cc", "/ranged.bin"); !bytes.Equal(got, big) {
		t.Fatal("ranged upload differs")
	}

	// Already a file: used as it is.
	f, err := os.CreateTemp(t.TempDir(), "src-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(big)
	f.Seek(0, io.SeekStart)
	n = measure(func() {
		fs := &stream.FileStream{Ctx: ctx, Obj: &model.Object{Name: "onfile.bin", Size: int64(len(big)), Modified: time.Now()}, Reader: f}
		if err := put(fs); err != nil {
			t.Fatalf("put of a file-backed stream: %v", err)
		}
	})
	f.Close()
	t.Logf("file-backed stream: %d cache files in the temp dir", n)
	if n != 0 {
		t.Fatalf("a file-backed stream was cached (%d files)", n)
	}
	if got := readAll(t, "/cc", "/onfile.bin"); !bytes.Equal(got, big) {
		t.Fatal("file upload differs")
	}

	// Read-once: one copy.
	n = measure(func() {
		if err := put(plainStream("once.bin", big, int64(len(big)))); err != nil {
			t.Fatalf("put of a one-shot stream: %v", err)
		}
	})
	t.Logf("one-shot stream: %d cache files in the temp dir", n)
	if n != 1 {
		t.Fatalf("a one-shot stream made %d cache files, want 1", n)
	}
	if got := readAll(t, "/cc", "/once.bin"); !bytes.Equal(got, big) {
		t.Fatal("cached upload differs")
	}
	if left, _ := os.ReadDir(temp); len(left) != 0 {
		t.Fatalf("%d files left in the temp dir after the uploads", len(left))
	}

	// Too big for the disk: a clear error, nothing written.
	const huge = 1 << 60
	n = measure(func() {
		err := put(plainStream("huge.bin", big, huge))
		if err == nil || !strings.Contains(err.Error(), "temp dir has") {
			t.Fatalf("a file bigger than the free space: %v", err)
		}
		t.Logf("error: %v", err)
	})
	if left, _ := os.ReadDir(temp); n != 0 || len(left) != 0 {
		t.Fatalf("temp dir after the refused upload: peak %d, %d files left", n, len(left))
	}
	if _, err := os.Stat(filepath.Join(under, "vol", "huge.bin")); err == nil {
		t.Fatal("the refused file reached the drive")
	}
}

// The SHA-1 the upper layer knows reaches the stream the underlying storage
// gets, so a drive that can take a file by hash can try that first.
func TestPutPassesSHA1Down(t *testing.T) {
	ctx := context.Background()
	withTempDir(t)
	under, state := t.TempDir(), t.TempDir()
	addStorage(t, "HashProbe", "/pu", `{"root_folder_path":`+q(under)+`}`)
	id := addStorage(t, "Cfs", "/pc", `{"remote_path":"/pu/vol","root_folder_path":"/","state_dir":`+q(state)+`,"seal_quiet":"50ms","seal_max":"500ms","threshold":"64KiB"}`)
	t.Cleanup(func() { op.DeleteStorageById(ctx, id) })
	st, err := op.GetStorageByMountPath("/pc")
	if err != nil {
		t.Fatal(err)
	}
	big := randBytes(3 << 20)
	if err := op.Put(ctx, st, "/", newSeekable(t, "known.bin", big, hexSHA1(big)), func(float64) {}); err != nil {
		t.Fatal(err)
	}
	got, ok := probed.Load("known.bin")
	if !ok || got.(string) != hexSHA1(big) {
		t.Fatalf("the underlying storage's stream carried SHA-1 %v (seen=%v), want %s", got, ok, hexSHA1(big))
	}
	// A stream without a known hash still goes through, with none.
	if err := op.Put(ctx, st, "/", plainStream("unknown.bin", big, int64(len(big))), func(float64) {}); err != nil {
		t.Fatal(err)
	}
	if got, _ := probed.Load("unknown.bin"); got != "" && got != nil {
		t.Logf("hash of a stream without one: %v", got)
	}
}
