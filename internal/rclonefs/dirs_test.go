package rclonefs_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/rclonefs"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/sync"
)

// noHookCtx keeps the after-write hook, which re-lists asynchronously, out of
// the call counts, and the test's errors in a stats group of its own: rclone
// refuses to delete in any later sync that sees errors in its group.
func noHookCtx(t *testing.T) context.Context {
	ctx := context.WithValue(context.Background(), conf.SkipHookKey, struct{}{})
	return accounting.WithStatsGroup(ctx, t.Name())
}

func wantContent(t *testing.T, d *fakedriver.Driver, path, want string) {
	t.Helper()
	if b, ok := d.Content(path); !ok || string(b) != want {
		t.Fatalf("%s = %q (exists=%v), want %q", path, b, ok, want)
	}
}

// A new directory costs one listing of its parent and one MakeDir, not
// op.MakeDir's lookups of the path and of its parent, each a rate-limit slot
// on drives like 115.
func TestNewTreeCostsAListingAndAMakeDirPerDir(t *testing.T) {
	ctx := noHookCtx(t)
	d := mountFake(t, "/tree")
	d.CloudLike = true
	src := t.TempDir()
	writeFile(t, src, "a/x.txt", "x")
	writeFile(t, src, "a/b/y.txt", "y")
	writeFile(t, src, "a/b/c/d/f.txt", "f")
	olFs, err := rclonefs.NewFs(ctx, "/tree/new")
	if err != nil {
		t.Fatal(err)
	}
	d.ResetCounts()
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("copy: %v", err)
	}
	t.Logf("gets=%d lists=%d mkdirs=%d puts=%d", d.Gets.Load(), d.Lists.Load(), d.MakeDirs.Load(), d.Puts.Load())
	wantContent(t, d, "/new/a/x.txt", "x")
	wantContent(t, d, "/new/a/b/y.txt", "y")
	wantContent(t, d, "/new/a/b/c/d/f.txt", "f")
	// new, a, b, c, d
	if m := d.MakeDirs.Load(); m != 5 {
		t.Fatalf("MakeDir called %d times, want 5", m)
	}
	// rclone's look at the missing destination; none per directory
	if g := d.Gets.Load(); g > 1 {
		t.Fatalf("Getter called %d times, want <= 1", g)
	}
	// only the storage root, which already was there, is listed: what is made
	// by this Fs is empty and needs no look
	if l := d.Lists.Load(); l > 1 {
		t.Fatalf("driver List called %d times, want <= 1", l)
	}
}

// Directories this Fs removed are made again on the next copy, even when the
// drive still lists them.
func TestRemovedDirsAreMadeAgainNotTakenFromAStaleListing(t *testing.T) {
	ctx := noHookCtx(t)
	d := mountFake(t, "/purge")
	d.CloudLike, d.LagDeletes = true, true
	src := t.TempDir()
	writeFile(t, src, "a/x.txt", "x")
	writeFile(t, src, "a/b/y.txt", "y")
	olFs, err := rclonefs.NewFs(ctx, "/purge/new")
	if err != nil {
		t.Fatal(err)
	}
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := operations.Purge(ctx, olFs, "a/b"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, ok := d.Content("/new/a/b/y.txt"); ok {
		t.Fatal("purge left a/b/y.txt")
	}
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("copy after purge: %v", err)
	}
	// The fake refuses a Put into a removed directory, so the file being
	// there means it went into a directory that exists.
	wantContent(t, d, "/new/a/b/y.txt", "y")

	// Removed and made again within one job, the same way.
	if err := operations.Purge(ctx, olFs, "a/b"); err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("copy after second purge: %v", err)
	}
	wantContent(t, d, "/new/a/b/y.txt", "y")
}

// A directory removed behind the Fs's back (another client, the web UI) is
// made again rather than written into.
func TestDirRemovedBehindTheFsIsMadeAgain(t *testing.T) {
	ctx := noHookCtx(t)
	d := mountFake(t, "/behind")
	d.CloudLike = true
	src := t.TempDir()
	writeFile(t, src, "a/b/y.txt", "y")
	olFs, err := rclonefs.NewFs(ctx, "/behind/new")
	if err != nil {
		t.Fatal(err)
	}
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("copy: %v", err)
	}
	st, err := op.GetStorageByMountPath("/behind")
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Remove(ctx, st, "/new/a/b"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	writeFile(t, src, "a/b/z.txt", "z")
	// The first run may fail on the stale directory; the next one must not.
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Logf("copy after the remove: %v", err)
		if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
			t.Fatalf("second copy after the remove: %v", err)
		}
	}
	wantContent(t, d, "/new/a/b/y.txt", "y")
	wantContent(t, d, "/new/a/b/z.txt", "z")
}

// The same, on a drive that still lists the removed directory.
func TestDirRemovedBehindTheFsOnALaggingDrive(t *testing.T) {
	ctx := noHookCtx(t)
	d := mountFake(t, "/lag")
	d.CloudLike, d.LagDeletes = true, true
	src := t.TempDir()
	writeFile(t, src, "a/b/y.txt", "y")
	olFs, err := rclonefs.NewFs(ctx, "/lag/new")
	if err != nil {
		t.Fatal(err)
	}
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("copy: %v", err)
	}
	st, err := op.GetStorageByMountPath("/lag")
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Remove(ctx, st, "/new/a/b"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	writeFile(t, src, "a/b/z.txt", "z")
	var lastErr error
	for run := 0; run < 3; run++ {
		if lastErr = sync.CopyDir(ctx, olFs, localFs(t, src), false); lastErr == nil {
			break
		}
		t.Logf("run %d: %v", run, lastErr)
	}
	if lastErr != nil {
		t.Fatalf("copy after the remove never succeeded: %v", lastErr)
	}
	wantContent(t, d, "/new/a/b/y.txt", "y")
	wantContent(t, d, "/new/a/b/z.txt", "z")
}

// A name another writer takes between the listing and MakeDir is used, not
// duplicated.
func TestDirMadeByAnotherWriterMeanwhileIsUsed(t *testing.T) {
	ctx := noHookCtx(t)
	d := mountFake(t, "/race")
	d.CloudLike = true
	d.AddDir("/new")
	taken := false
	d.OnMakeDir = func(path string) {
		if path == "/new/a" && !taken {
			taken = true
			d.AddDir("/new/a")
		}
	}
	src := t.TempDir()
	writeFile(t, src, "a/x.txt", "x")
	olFs, err := rclonefs.NewFs(ctx, "/race/new")
	if err != nil {
		t.Fatal(err)
	}
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if !taken {
		t.Fatal("the other writer never ran")
	}
	wantContent(t, d, "/new/a/x.txt", "x")
}

// A file where a directory is wanted is reported, not replaced or wrapped.
func TestFileInTheWayOfADirIsAnError(t *testing.T) {
	ctx := noHookCtx(t)
	d := mountFake(t, "/clash")
	d.CloudLike = true
	d.AddDir("/new")
	d.AddFile("/new/a", "not a dir")
	src := t.TempDir()
	writeFile(t, src, "a/x.txt", "x")
	olFs, err := rclonefs.NewFs(ctx, "/clash/new")
	if err != nil {
		t.Fatal(err)
	}
	if err := olFs.Mkdir(ctx, "a"); err == nil {
		t.Fatal("Mkdir over a file succeeded")
	}
	wantContent(t, d, "/new/a", "not a dir")
}

func putString(ctx context.Context, f *rclonefs.Fs, remote, content string) error {
	info := object.NewStaticObjectInfo(remote, time.Now(), int64(len(content)), true, nil, nil)
	_, err := f.Put(ctx, strings.NewReader(content), info)
	return err
}

// Making many sibling directories in a parent that already holds many entries
// lists the parent once, not once per directory (on 115 a listing is a page
// per 1000 entries at one request a second).
func TestSiblingDirsInAnExistingParentListItOnce(t *testing.T) {
	ctx := noHookCtx(t)
	d := mountFake(t, "/wide")
	d.CloudLike = true
	d.AddDir("/new")
	for i := 0; i < 20; i++ {
		d.AddFile(fmt.Sprintf("/new/old%02d.txt", i), "old")
	}
	olFs, err := rclonefs.NewFs(ctx, "/wide/new")
	if err != nil {
		t.Fatal(err)
	}
	d.ResetCounts()
	for i := 0; i < 6; i++ {
		if err := putString(ctx, olFs, fmt.Sprintf("d%d/x.txt", i), "x"); err != nil {
			t.Fatalf("put into d%d: %v", i, err)
		}
	}
	t.Logf("lists of /new=%d mkdirs=%d", d.ListCount("/new"), d.MakeDirs.Load())
	for i := 0; i < 6; i++ {
		wantContent(t, d, fmt.Sprintf("/new/d%d/x.txt", i), "x")
	}
	if n := d.MakeDirs.Load(); n != 6 {
		t.Fatalf("MakeDir called %d times, want 6", n)
	}
	if n := d.ListCount("/new"); n > 1 {
		t.Fatalf("/new listed %d times for 6 new siblings, want <= 1", n)
	}
	// A name that turns out to be taken makes the next one look afresh.
	d.AddDir("/new/late")
	if err := putString(ctx, olFs, "late/y.txt", "y"); err != nil {
		t.Fatalf("put into late: %v", err)
	}
	wantContent(t, d, "/new/late/y.txt", "y")
}

// A driver that answers ObjectNotFound for a file it did store doesn't make
// the directory it is in a removed one: it is used again, not made again.
func TestNotFoundAfterAPutDoesNotBuryTheDir(t *testing.T) {
	ctx := noHookCtx(t)
	d := mountFake(t, "/nf")
	d.CloudLike = true
	d.PutReportsNotFound = true
	olFs, err := rclonefs.NewFs(ctx, "/nf/new")
	if err != nil {
		t.Fatal(err)
	}
	if err := olFs.Mkdir(ctx, "a"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	d.ResetCounts()
	for i := 0; i < 3; i++ {
		// the error is the driver's; where the file went is what matters
		_ = putString(ctx, olFs, fmt.Sprintf("a/f%d.txt", i), "f")
	}
	for i := 0; i < 3; i++ {
		wantContent(t, d, fmt.Sprintf("/new/a/f%d.txt", i), "f")
	}
	if n := d.MakeDirs.Load(); n != 0 {
		t.Fatalf("MakeDir called %d times for a directory that was there, want 0", n)
	}
}

// On a drive whose IDs are paths, a directory someone else made again where
// this Fs removed one has the removed one's ID. It is still taken as it is
// found, not looked for again for every file.
func TestDirMadeAgainByAnotherWriterAtTheSamePath(t *testing.T) {
	ctx := noHookCtx(t)
	d := mountFake(t, "/pid")
	d.CloudLike, d.PathIDs = true, true
	src := t.TempDir()
	writeFile(t, src, "a/b/y.txt", "y")
	olFs, err := rclonefs.NewFs(ctx, "/pid/new")
	if err != nil {
		t.Fatal(err)
	}
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := operations.Purge(ctx, olFs, "a/b"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	d.AddDir("/new/a/b")
	d.ResetCounts()
	for i := 0; i < 3; i++ {
		if err := putString(ctx, olFs, fmt.Sprintf("a/b/f%d.txt", i), "f"); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	t.Logf("gets=%d lists=%d mkdirs=%d", d.Gets.Load(), d.Lists.Load(), d.MakeDirs.Load())
	for i := 0; i < 3; i++ {
		wantContent(t, d, fmt.Sprintf("/new/a/b/f%d.txt", i), "f")
	}
	if n := d.MakeDirs.Load(); n > 1 {
		t.Fatalf("MakeDir called %d times for 3 files, want <= 1", n)
	}
	if n := d.Lists.Load(); n > 1 {
		t.Fatalf("driver List called %d times for 3 files, want <= 1", n)
	}
}
