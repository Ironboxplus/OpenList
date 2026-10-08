package op_test

import (
	"context"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
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

func newFile(name, content string) *stream.FileStream {
	return &stream.FileStream{
		Obj:    &model.Object{Name: name, Size: int64(len(content)), HashInfo: utils.NewHashInfo(nil, "")},
		Reader: strings.NewReader(content),
	}
}

func TestPutInDirDoesNoLookupsAndUpdatesDirCache(t *testing.T) {
	// The after-write hook re-lists asynchronously; keep it out of the counts.
	ctx := context.WithValue(context.Background(), conf.SkipHookKey, struct{}{})
	d := mountFake(t, "/putindir")
	d.AddDir("/d")
	d.AddFile("/d/seed.txt", "s") // op only caches non-empty listings

	// Warm the dir cache and get the parent object the way a caller would.
	if _, err := op.List(ctx, d, "/d", model.ListArgs{}); err != nil {
		t.Fatal(err)
	}
	parent, err := op.GetUnwrap(ctx, d, "/d")
	if err != nil {
		t.Fatal(err)
	}
	d.ResetCounts()

	if err := op.PutInDir(ctx, d, parent, "/d", newFile("a.txt", "hello"), nil); err != nil {
		t.Fatalf("PutInDir: %v", err)
	}
	if got := d.Gets.Load(); got != 0 {
		t.Fatalf("PutInDir called Getter %d times, want 0", got)
	}
	if d.Lists.Load() != 0 || d.MakeDirs.Load() != 0 || d.Puts.Load() != 1 {
		t.Fatalf("lists=%d mkdirs=%d puts=%d, want 0/0/1", d.Lists.Load(), d.MakeDirs.Load(), d.Puts.Load())
	}
	if b, ok := d.Content("/d/a.txt"); !ok || string(b) != "hello" {
		t.Fatalf("stored content = %q (exists=%v)", b, ok)
	}

	// The dir cache holds the new file: listing again hits the cache.
	objs, err := op.List(ctx, d, "/d", model.ListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Lists.Load() != 0 {
		t.Fatalf("List after PutInDir went to the driver %d times, want cache hit", d.Lists.Load())
	}
	if len(objs) != 2 {
		t.Fatalf("cached listing = %v, want seed.txt and a.txt", objs)
	}
}

func TestPutInDirRejectsNoWriteParent(t *testing.T) {
	ctx := context.Background()
	d := mountFake(t, "/putindir-nowrite")
	parent := &model.Object{Name: "d", Path: "/d", IsFolder: true, Mask: model.NoWrite}
	if err := op.PutInDir(ctx, d, parent, "/d", newFile("a.txt", "x"), nil); err == nil {
		t.Fatal("PutInDir into a NoWrite dir succeeded")
	}
	if d.Puts.Load() != 0 {
		t.Fatal("driver Put was called")
	}
}

func TestPutStillCreatesMissingDirsAndOverwrites(t *testing.T) {
	ctx := context.Background()
	d := mountFake(t, "/put-regress")

	if err := op.Put(ctx, d, "/x/y", newFile("f.txt", "one"), nil); err != nil {
		t.Fatalf("Put into missing dir: %v", err)
	}
	if b, ok := d.Content("/x/y/f.txt"); !ok || string(b) != "one" {
		t.Fatalf("content = %q (exists=%v)", b, ok)
	}
	if err := op.Put(ctx, d, "/x/y", newFile("f.txt", "two"), nil); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	if b, _ := d.Content("/x/y/f.txt"); string(b) != "two" {
		t.Fatalf("content after overwrite = %q", b)
	}
}
