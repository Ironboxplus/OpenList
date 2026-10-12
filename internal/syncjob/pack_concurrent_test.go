package syncjob_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/syncjob"
	rfs "github.com/rclone/rclone/fs"
)

// countFs counts the Lists made through it and tells when the job manifest is
// first put.
type countFs struct {
	rfs.Fs
	features *rfs.Features
	lists    *atomic.Int32
	onCommit func()
}

func newCountFs(inner rfs.Fs, lists *atomic.Int32, onCommit func()) *countFs {
	c := &countFs{Fs: inner, lists: lists, onCommit: onCommit}
	// Rebuilt from c so nothing (ListR, ...) goes around List.
	c.features = (&rfs.Features{CanHaveEmptyDirectories: inner.Features().CanHaveEmptyDirectories}).Fill(context.Background(), c)
	return c
}

func (c *countFs) Features() *rfs.Features { return c.features }

func (c *countFs) List(ctx context.Context, dir string) (rfs.DirEntries, error) {
	c.lists.Add(1)
	return c.Fs.List(ctx, dir)
}

func (c *countFs) Put(ctx context.Context, in io.Reader, src rfs.ObjectInfo, opts ...rfs.OpenOption) (rfs.Object, error) {
	if src.Remote() == ".oppack/manifest.json.gz" && c.onCommit != nil {
		c.onCommit()
	}
	return c.Fs.Put(ctx, in, src, opts...)
}

// The packing stage reads src and the manifest only: what it asks of dst
// doesn't grow with the number of directories, and the main pass's listings
// are the only walk of dst there is.
func TestPackAsksDstNothingPerDirectory(t *testing.T) {
	m := newManager(t)
	var atCommit []int32
	for _, n := range []int{50, 200} {
		src, dst := tempDir(t), tempDir(t)
		for d := 0; d < n; d++ {
			write(t, src, fmt.Sprintf("dir%03d/only.json", d), fmt.Sprintf(`{"d":%d}`, d))
		}
		// dst has the directories already, so the main pass lists each one.
		for d := 0; d < n; d++ {
			if err := os.MkdirAll(filepath.Join(dst, fmt.Sprintf("dir%03d", d)), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		var lineLists, mainLists, beforeCommit atomic.Int32
		beforeCommit.Store(-1)
		restore := syncjob.SetDstWrapper(func(f rfs.Fs, packLine bool) rfs.Fs {
			if packLine {
				return newCountFs(f, &lineLists, func() { beforeCommit.CompareAndSwap(-1, lineLists.Load()) })
			}
			return newCountFs(f, &mainLists, nil)
		})
		s := mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, syncjob.PackOptions{Threshold: 1024})))
		restore()
		if s.PackStats.PackedFiles != int64(n) || s.PackStats.PacksWritten != 1 {
			t.Fatalf("N=%d: stats %+v", n, s.PackStats)
		}
		t.Logf("N=%d: pack-line dst Lists before commit=%d, in all=%d; main pass Lists=%d", n, beforeCommit.Load(), lineLists.Load(), mainLists.Load())
		if got := beforeCommit.Load(); got < 0 || got > 2 {
			t.Fatalf("N=%d: %d dst Lists before the commit, want at most 2", n, got)
		}
		// Nothing but the main pass walks dst.
		if lineLists.Load() > 2 {
			t.Fatalf("N=%d: %d dst Lists beside the main pass's %d", n, lineLists.Load(), mainLists.Load())
		}
		atCommit = append(atCommit, beforeCommit.Load())
	}
	if atCommit[0] != atCommit[1] {
		t.Fatalf("Lists before the commit depend on the number of directories: %v", atCommit)
	}
}

// Large files don't wait for the pack line: with the line held back, the
// first large file lands at dst, and no manifest does yet.
func TestPackLineAndLargeFilesRunTogether(t *testing.T) {
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	write(t, src, "a/big.bin", strings.Repeat("B", 4000))
	for i := 0; i < 3; i++ {
		write(t, src, fmt.Sprintf("a/s%d.json", i), "{}")
	}
	var bigFirst atomic.Bool
	restore := syncjob.SetPackLineStart(func(context.Context) {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if exists(dst, "a/big.bin") {
				bigFirst.Store(!exists(dst, ".oppack/manifest.json.gz"))
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	defer restore()
	s := mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, syncjob.PackOptions{Threshold: 1024})))
	if !bigFirst.Load() {
		t.Fatalf("the large file did not reach dst while the pack line was still held back")
	}
	if len(loadTestManifest(t, dst).Files) != 3 || s.Stage != "transferring" {
		t.Fatalf("manifest or stage wrong: stage %q", s.Stage)
	}
	if got, _ := read(t, dst, "a/big.bin"); len(got) != 4000 {
		t.Fatalf("large file size %d", len(got))
	}
}

// A pack that fails to go up fails the job, with the large files' pass
// stopped along; the old manifest stays valid and the next run recovers.
func TestPackFailureAlongsideLargeFiles(t *testing.T) {
	m := newManager(t)
	src := tempDir(t)
	for i := 0; i < 3; i++ {
		write(t, src, fmt.Sprintf("d/f%d.txt", i), fmt.Sprintf("v1 %d", i))
	}
	write(t, src, "d/big.bin", strings.Repeat("B", 3000))
	drv := fakeMount(t, "/fk-concurrent")
	spec := syncjob.Spec{Mode: syncjob.ModeSync, Src: "local:" + src, Dst: "/fk-concurrent",
		Pack: &syncjob.PackOptions{Enabled: true, Threshold: 1024, MaxPackBytes: 1}}
	mustSucceed(t, runJob(t, m, spec))
	time.Sleep(200 * time.Millisecond)
	before, ok := drv.Content("/.oppack/manifest.json.gz")
	if !ok {
		t.Fatal("no manifest after the first run")
	}

	write(t, src, "d/f1.txt", "v2 edited")
	write(t, src, "d/big2.bin", strings.Repeat("C", 3000))
	drv.FailPut = func(path string) error {
		if strings.HasPrefix(path, "/.oppack/p-") {
			return fmt.Errorf("injected failure on %s", path)
		}
		return nil
	}
	s := runJob(t, m, spec)
	if s.State != syncjob.StateFailed || !strings.Contains(s.Error, "injected failure") {
		t.Fatalf("job state = %s (%s), want failed by the injected error", s.State, s.Error)
	}
	if after, _ := drv.Content("/.oppack/manifest.json.gz"); !bytes.Equal(before, after) {
		t.Fatalf("manifest changed although the pack line failed")
	}

	drv.FailPut = nil
	mustSucceed(t, runJob(t, m, spec))
	time.Sleep(200 * time.Millisecond)
	if after, _ := drv.Content("/.oppack/manifest.json.gz"); bytes.Equal(before, after) {
		t.Fatalf("the retry did not commit")
	}
	if _, ok := drv.Content("/d/big2.bin"); !ok {
		t.Fatalf("the large file never arrived")
	}
}

// Members that only a v1 pack has are moved over in a second commit, and the
// manifest it replaces is the first commit's.
func TestPackV1MigrantsGetASecondCommit(t *testing.T) {
	mt := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	write(t, src, "old/dir/a.json", `{"a":1}`)
	write(t, src, "old/dir/b.json", `{"b":2}`)
	buildV1(t, dst, "old/dir", map[string]string{"a.json": `{"a":1}`, "b.json": `{"b":2}`, "c.json": `{"c":3}`}, mt)

	s := mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, dst, syncjob.PackOptions{Threshold: 1024})))
	if exists(dst, "old/dir/.oppack") {
		t.Fatalf("v1 directory left")
	}
	if s.PackStats.V1MigratedDirs != 1 || s.PackStats.PacksWritten != 2 {
		t.Fatalf("stats = %+v", s.PackStats)
	}
	final := loadTestManifest(t, dst)
	if got := strings.Join(fileNames(final), ","); got != "old/dir/a.json,old/dir/b.json,old/dir/c.json" {
		t.Fatalf("final manifest files = %s", got)
	}
	prev := readTestManifest(t, dst, "manifest.prev.json.gz")
	if got := strings.Join(fileNames(prev), ","); got != "old/dir/a.json,old/dir/b.json" {
		t.Fatalf("manifest.prev should be the first commit; files = %s", got)
	}
	for name := range final.Packs {
		if !exists(dst, filepath.ToSlash(filepath.Join(".oppack", name))) {
			t.Fatalf("manifest lists %s which is not at dst", name)
		}
	}
}

// putGateFs holds each pack's upload for a moment and counts how many are in
// flight at once.
type putGateFs struct {
	rfs.Fs
	features      *rfs.Features
	inFlight, max *atomic.Int32
}

func newPutGateFs(inner rfs.Fs, inFlight, max *atomic.Int32) *putGateFs {
	c := &putGateFs{Fs: inner, inFlight: inFlight, max: max}
	c.features = (&rfs.Features{CanHaveEmptyDirectories: inner.Features().CanHaveEmptyDirectories}).Fill(context.Background(), c)
	return c
}

func (c *putGateFs) Features() *rfs.Features { return c.features }

func (c *putGateFs) Put(ctx context.Context, in io.Reader, src rfs.ObjectInfo, opts ...rfs.OpenOption) (rfs.Object, error) {
	if strings.HasSuffix(src.Remote(), ".zip") {
		n := c.inFlight.Add(1)
		defer c.inFlight.Add(-1)
		for {
			m := c.max.Load()
			if n <= m || c.max.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return c.Fs.Put(ctx, in, src, opts...)
}

// Packs go up side by side, as many as the job's transfers, while the next
// one is being built; the manifest still lists every one of them.
func TestPackUploadsRunSideBySide(t *testing.T) {
	m := newManager(t)
	src, dst := tempDir(t), tempDir(t)
	const packs = 8
	for i := 0; i < packs; i++ {
		write(t, src, fmt.Sprintf("d/s%d.json", i), strings.Repeat("x", 300+i))
	}
	var inFlight, peak atomic.Int32
	restore := syncjob.SetDstWrapper(func(f rfs.Fs, packLine bool) rfs.Fs {
		if packLine {
			return newPutGateFs(f, &inFlight, &peak)
		}
		return f
	})
	defer restore()
	spec := packSpec(syncjob.ModeCopy, src, dst, syncjob.PackOptions{Threshold: 1024, MaxPackBytes: 1})
	spec.Transfers = 3
	s := mustSucceed(t, runJob(t, m, spec))
	if s.PackStats.PacksWritten != packs {
		t.Fatalf("stats %+v, want %d packs", s.PackStats, packs)
	}
	if got := peak.Load(); got != 3 {
		t.Fatalf("at most %d packs uploaded at once, want 3 (the job's transfers)", got)
	}
	if mf := loadTestManifest(t, dst); len(mf.Files) != packs || len(mf.Packs) != packs {
		t.Fatalf("manifest has %d files in %d packs, want %d each", len(mf.Files), len(mf.Packs), packs)
	}
}
