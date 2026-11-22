package cfs_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	cfsdrv "github.com/OpenListTeam/OpenList/v4/drivers/cfs"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	cfsrclone "github.com/Ironboxplus/cfs/frontend/rclone"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

// streamCounter counts the range streams a drive is asked to open for each
// file: how many were opened, and the most that were open at the same time.
type streamCounter struct {
	mu                sync.Mutex
	opened, open, max map[string]int
}

func newStreamCounter() *streamCounter {
	return &streamCounter{opened: map[string]int{}, open: map[string]int{}, max: map[string]int{}}
}

func (c *streamCounter) onOpen(path string, _ int64) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opened[path]++
	c.open[path]++
	c.max[path] = max(c.max[path], c.open[path])
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.open[path]--
	}
}

// of returns the counts of the one file whose path ends in suffix.
func (c *streamCounter) of(t *testing.T, suffix string) (opened, peak int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	found := ""
	for p := range c.opened {
		if strings.HasSuffix(p, suffix) {
			if found != "" {
				t.Fatalf("both %s and %s match %s", found, p, suffix)
			}
			found = p
		}
	}
	return c.opened[found], c.max[found]
}

// readLikeAServer reads the file front to back the way the NFS and FUSE
// servers do: for each chunk it opens the file, reads at the offset, and closes
// it again. It returns the SHA-256 of what it read.
func readLikeAServer(t *testing.T, v *vfs.VFS, name string, size int64) [sha256.Size]byte {
	t.Helper()
	h := sha256.New()
	buf := make([]byte, 64<<10)
	for off := int64(0); off < size; {
		fd, err := v.OpenFile(name, os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("open at %d: %v", off, err)
		}
		n, err := fd.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("read at %d: %v", off, err)
		}
		if n == 0 {
			t.Fatalf("read at %d returned no data", off)
		}
		if err := fd.Close(); err != nil {
			t.Fatalf("close at %d: %v", off, err)
		}
		h.Write(buf[:n])
		off += int64(n)
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}

// What `openlist cfs mount` and `cfs serve nfs` put in front of a big file
// that is stored 1:1 on a cloud drive: rclone's VFS over the cfs backend over
// the volume of a Cfs storage over an OpenList storage. Read in 64 KiB pieces,
// each with its own open, as the kernel's FUSE and NFS clients make the server
// do, the drive sees one range stream per piece with --vfs-cache-mode writes.
// With full the file has one cache entry and one sequential downloader, and the
// drive sees one stream for the whole file. A drive like 115 refuses a second
// stream to the same file, which is why the mount uses full.
func TestVfsReadsOfABigFileOpenFewStreams(t *testing.T) {
	ctx := context.Background()
	const size = 32 << 20 // above the 8 MiB threshold: stored 1:1
	data := randBytes(size)
	want := sha256.Sum256(data)

	underID := addStorage(t, fakedriver.DriverName, "/vfsread-under", "{}")
	under, err := op.GetStorageByMountPath("/vfsread-under")
	if err != nil {
		t.Fatal(err)
	}
	drive := under.(*fakedriver.Driver)
	drive.CloudLike = true
	drive.AddDir("/vol")
	cfsID := addStorage(t, "Cfs", "/vfsread-cfs", `{"remote_path":"/vfsread-under/vol","root_folder_path":"/","state_dir":`+q(t.TempDir())+`}`)
	t.Cleanup(func() {
		op.DeleteStorageById(context.Background(), cfsID)
		op.DeleteStorageById(context.Background(), underID)
	})
	putFile(t, "/vfsread-cfs", "/", "big.bin", data)

	for _, mode := range []vfscommon.CacheMode{vfscommon.CacheModeWrites, vfscommon.CacheModeFull} {
		t.Run(mode.String(), func(t *testing.T) {
			// Each run has a VFS and a cache directory of its own.
			if err := config.SetCacheDir(t.TempDir()); err != nil {
				t.Fatal(err)
			}
			sh := cfsrclone.Lookup(cfsdrv.ShareKey("/vfsread-cfs"))
			if sh == nil {
				t.Fatal("the storage's volume is not registered")
			}
			f, err := sh.NewFs(ctx, "cfs", "")
			if err != nil {
				t.Fatal(err)
			}
			opt := vfscommon.Opt
			opt.CacheMode = mode
			v := vfs.New(ctx, f, &opt)
			t.Cleanup(func() {
				v.Shutdown()
				f.(fs.Shutdowner).Shutdown(ctx)
				sh.Release(ctx)
			})

			counter := newStreamCounter()
			drive.OnRangeOpen = counter.onOpen
			t.Cleanup(func() { drive.OnRangeOpen = nil })

			if got := readLikeAServer(t, v, "big.bin", size); got != want {
				t.Fatal("the bytes read back differ from the bytes written")
			}
			opened, peak := counter.of(t, "/big.bin")
			t.Errorf("TEMP-MEASURE cache mode %s: %d range streams opened for the file, at most %d at once", mode, opened, peak)

			switch mode {
			case vfscommon.CacheModeFull:
				if peak != 1 {
					t.Errorf("%d streams were open at once for one file, want 1", peak)
				}
				if opened > 4 {
					t.Errorf("%d streams were opened for one sequential read, want at most 4", opened)
				}
			default:
				if opened <= 100 {
					t.Errorf("%d streams were opened, expected more than 100 (about one per read)", opened)
				}
			}
		})
	}
}
