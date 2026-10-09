package syncjob_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/syncjob"
	rfs "github.com/rclone/rclone/fs"
)

// mkdirFs counts the Mkdir calls rclone makes through it, apart from those
// under .oppack.
type mkdirFs struct {
	rfs.Fs
	features *rfs.Features
	mkdirs   *atomic.Int32
}

func newMkdirFs(inner rfs.Fs, n *atomic.Int32) *mkdirFs {
	c := &mkdirFs{Fs: inner, mkdirs: n}
	c.features = (&rfs.Features{CanHaveEmptyDirectories: inner.Features().CanHaveEmptyDirectories}).Fill(context.Background(), c)
	return c
}

func (c *mkdirFs) Features() *rfs.Features { return c.features }

func (c *mkdirFs) Mkdir(ctx context.Context, dir string) error {
	if dir != ".oppack" && !strings.HasPrefix(dir, ".oppack/") {
		c.mkdirs.Add(1)
	}
	return c.Fs.Mkdir(ctx, dir)
}

// dirsOf lists every directory under root, slash separated.
func dirsOf(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// Directories whose files all live in packs are not created at dst: what
// dst gets is the large files' directories, the genuinely empty directories
// and the parents of those; the number of Mkdirs does not grow with the
// number of pack-only directories.
func TestPackOnlyDirectoriesAreNotCreatedAtDst(t *testing.T) {
	m := newManager(t)
	var counts []int32
	for _, n := range []int{10, 40} {
		src, dst := tempDir(t), tempDir(t)
		for i := 0; i < n; i++ {
			write(t, src, fmt.Sprintf("p%03d/sub/deep/a.json", i), `{"a":1}`)
			write(t, src, fmt.Sprintf("p%03d/sub/b.json", i), `{"b":2}`)
		}
		write(t, src, "big/x.bin", strings.Repeat("B", 3000))
		// pk holds only small files, mix has a large one beside it.
		write(t, src, "mix/pk/s.json", "{}")
		write(t, src, "mix/big.bin", strings.Repeat("M", 3000))
		// Empty leaves are recreated, and so are the parents on the way.
		if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, src, "q/sub/s.json", "{}")
		if err := os.MkdirAll(filepath.Join(src, "q/sub/leaf"), 0o755); err != nil {
			t.Fatal(err)
		}

		var mkdirs atomic.Int32
		restore := syncjob.SetDstWrapper(func(f rfs.Fs, packLine bool) rfs.Fs {
			if packLine {
				return f
			}
			return newMkdirFs(f, &mkdirs)
		})
		for run := 0; run < 2; run++ {
			mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeSync, src, dst, syncjob.PackOptions{Threshold: 1024})))
			got := strings.Join(dirsOf(t, dst), ",")
			if want := ".oppack,big,empty,mix,q,q/sub,q/sub/leaf"; got != want {
				t.Fatalf("N=%d run %d: dst dirs = %s\nwant %s", n, run, got, want)
			}
		}
		restore()
		if got := len(loadTestManifest(t, dst).Files); got != 2*n+2 {
			t.Fatalf("N=%d: %d files packed", n, got)
		}
		t.Logf("N=%d: dst Mkdir calls (two runs) = %d", n, mkdirs.Load())
		counts = append(counts, mkdirs.Load())
	}
	if counts[0] != counts[1] {
		t.Fatalf("Mkdir calls grow with the number of pack-only directories: %v", counts)
	}
}

// A pack-only directory that dst already has from an earlier non-pack run.
// d holds the loose copies of what src has plus two files src never had; e
// holds loose copies only.
//
// sync, before directories were hidden: d existed in both views (src showed
// it as empty), so the foreign files were purged as extras and d itself stayed;
// e stayed untouched. Now d is dst-only to rclone, which purges the same
// foreign files, and its attempt to rmdir d fails quietly while the loose
// copies are there; e is hidden on both sides. The loose copies go in the
// tail either way. copy never deletes, so it keeps the foreign files.
func TestPackOnlyDirectoryAtDstKeepsSyncSemantics(t *testing.T) {
	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, mode := range []syncjob.Mode{syncjob.ModeCopy, syncjob.ModeSync} {
		m := newManager(t)
		src, dst := tempDir(t), tempDir(t)
		for _, rel := range []string{"d/a.json", "d/b.json", "e/sub/c.json"} {
			write(t, src, rel, "content "+rel)
			setMTime(t, src, rel, when)
			write(t, dst, rel, "content "+rel)
			setMTime(t, dst, rel, when)
		}
		write(t, dst, "d/foreign.txt", "not from src")
		write(t, dst, "d/extra/foreign2.txt", "not from src either")

		s := mustSucceed(t, runJob(t, m, packSpec(mode, src, dst, syncjob.PackOptions{Threshold: 1024})))
		for _, rel := range []string{"d/a.json", "d/b.json", "e/sub/c.json"} {
			if exists(dst, rel) {
				t.Fatalf("%s: loose copy %s kept", mode, rel)
			}
		}
		if s.PackStats.LooseRemoved != 3 || s.PackStats.Conflicts != 0 {
			t.Fatalf("%s: stats = %+v", mode, s.PackStats)
		}
		wantForeign := mode == syncjob.ModeCopy
		for _, rel := range []string{"d/foreign.txt", "d/extra/foreign2.txt"} {
			if exists(dst, rel) != wantForeign {
				t.Fatalf("%s: foreign file %s present=%v, want %v", mode, rel, exists(dst, rel), wantForeign)
			}
		}
		if len(loadTestManifest(t, dst).Files) != 3 {
			t.Fatalf("%s: not all files packed", mode)
		}
	}
}

// Loose copies and v1 .oppack directories inside a directory that is hidden
// from the main pass are still found: the former retired, the latter migrated.
func TestPackOnlyDirectoryStillRetiresLooseAndMigratesV1(t *testing.T) {
	mt := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	for _, mode := range []syncjob.Mode{syncjob.ModeCopy, syncjob.ModeSync} {
		m := newManager(t)
		src, dst := tempDir(t), tempDir(t)
		write(t, src, "v/dir/a.json", `{"a":1}`)
		write(t, src, "v/dir/b.json", `{"b":2}`)
		write(t, src, "v/other/z.json", `{"z":1}`)
		setMTime(t, src, "v/other/z.json", mt)
		// Only in the v1 pack at v/dir; z.json is loose at dst, below v/.
		buildV1(t, dst, "v/dir", map[string]string{"a.json": `{"a":1}`, "b.json": `{"b":2}`, "c.json": `{"c":3}`}, mt)
		write(t, dst, "v/other/z.json", `{"z":1}`)
		setMTime(t, dst, "v/other/z.json", mt)

		s := mustSucceed(t, runJob(t, m, packSpec(mode, src, dst, syncjob.PackOptions{Threshold: 1024})))
		if exists(dst, "v/dir/.oppack") || exists(dst, "v/other/z.json") {
			t.Fatalf("%s: v1 directory or loose copy left", mode)
		}
		if s.PackStats.V1MigratedDirs != 1 || s.PackStats.LooseRemoved != 1 {
			t.Fatalf("%s: stats = %+v", mode, s.PackStats)
		}
		want := "v/dir/a.json,v/dir/b.json,v/dir/c.json,v/other/z.json"
		if mode == syncjob.ModeSync {
			want = "v/dir/a.json,v/dir/b.json,v/other/z.json"
		}
		if got := strings.Join(fileNames(loadTestManifest(t, dst)), ","); got != want {
			t.Fatalf("%s: manifest files = %s\nwant %s", mode, got, want)
		}
	}
}

// Packing then unpacking gives back the same tree, the genuinely empty
// directories included.
func TestPackOnlyDirectoriesRoundTrip(t *testing.T) {
	m := newManager(t)
	src, packed, out := tempDir(t), tempDir(t), tempDir(t)
	buildRoundTripTree(t, src)
	write(t, src, "only/small/deep/a.json", `{"a":1}`)
	write(t, src, "only/small/b.json", `{"b":1}`)
	for _, d := range []string{"empty", "only/small/empty-leaf", "only/empty2"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustSucceed(t, runJob(t, m, packSpec(syncjob.ModeCopy, src, packed, syncjob.PackOptions{Threshold: 1024})))
	if exists(packed, "only/small/deep") || exists(packed, "few") {
		t.Fatalf("pack-only directories were created at dst")
	}
	mustSucceed(t, runJob(t, m, unpackSpec(syncjob.ModeCopy, packed, out)))
	sameTree(t, tree(t, src), tree(t, out))
	if want, got := strings.Join(dirsOf(t, src), ","), strings.Join(dirsOf(t, out), ","); want != got {
		t.Fatalf("directories differ\nsrc: %s\nout: %s", want, got)
	}
}
