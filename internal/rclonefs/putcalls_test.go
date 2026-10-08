package rclonefs_test

import (
	"context"
	"fmt"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/rclonefs"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	"github.com/rclone/rclone/fs/sync"
)

func mountFake(t *testing.T, mount string) *fakedriver.Driver {
	t.Helper()
	id, err := op.CreateStorage(context.Background(), model.Storage{Driver: fakedriver.DriverName, MountPath: mount, Addition: "{}"})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	s, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatal(err)
	}
	return s.(*fakedriver.Driver)
}

// A driver whose Get costs a rate-limit slot (115) must not be asked about
// the destination directory once per uploaded file, however stale op's own
// dir cache is.
func TestCopyNewFilesDoesNotLookUpPerFile(t *testing.T) {
	// The after-write hook re-lists asynchronously; keep it out of the counts.
	ctx := context.WithValue(context.Background(), conf.SkipHookKey, struct{}{})
	d := mountFake(t, "/calls")
	src := t.TempDir()
	for i := 0; i < 10; i++ {
		writeFile(t, src, fmt.Sprintf("f%02d.txt", i), fmt.Sprintf("content %d", i))
	}
	olFs, err := rclonefs.NewFs(ctx, "/calls/new")
	if err != nil {
		t.Fatal(err)
	}
	d.ResetCounts()
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("copy: %v", err)
	}
	t.Logf("first batch: gets=%d lists=%d mkdirs=%d puts=%d", d.Gets.Load(), d.Lists.Load(), d.MakeDirs.Load(), d.Puts.Load())
	if d.Puts.Load() != 10 {
		t.Fatalf("puts = %d, want 10", d.Puts.Load())
	}
	if b, ok := d.Content("/new/f07.txt"); !ok || string(b) != "content 7" {
		t.Fatalf("f07.txt = %q (exists=%v)", b, ok)
	}
	// Constant in the file count: rclone's initial listing of the destination,
	// then MakeDir's existence check and the lookup of the created directory.
	if g := d.Gets.Load(); g > 3 {
		t.Fatalf("Getter called %d times for 10 new files, want <= 3", g)
	}
	if l := d.Lists.Load(); l > 2 {
		t.Fatalf("driver List called %d times for 10 new files, want no per-file listing", l)
	}

	// op's dir cache expires mid-job.
	op.Cache.ClearAll()
	for i := 10; i < 20; i++ {
		writeFile(t, src, fmt.Sprintf("f%02d.txt", i), fmt.Sprintf("content %d", i))
	}
	d.ResetCounts()
	if err := sync.CopyDir(ctx, olFs, localFs(t, src), false); err != nil {
		t.Fatalf("second copy: %v", err)
	}
	t.Logf("after cache expiry: gets=%d lists=%d mkdirs=%d puts=%d", d.Gets.Load(), d.Lists.Load(), d.MakeDirs.Load(), d.Puts.Load())
	if d.Puts.Load() != 10 {
		t.Fatalf("puts after expiry = %d, want 10", d.Puts.Load())
	}
	// The job's Fs remembers the directory, so op's expiry costs nothing.
	if g := d.Gets.Load(); g > 1 {
		t.Fatalf("Getter called %d times after cache expiry, want <= 1 (the listing)", g)
	}
	if l := d.Lists.Load(); l > 2 {
		t.Fatalf("driver List called %d times after cache expiry, want <= 2", l)
	}
}
