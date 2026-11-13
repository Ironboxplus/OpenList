package rclonefs_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/rclonefs"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	"github.com/rclone/rclone/fs"
)

// mountCloudLike mounts a fake drive that, like a cloud drive, gives every file
// its own id and whose links stay on the content they were made for, and that
// caches listings and links in op.
func mountCloudLike(t *testing.T, mount string) *fakedriver.Driver {
	t.Helper()
	ctx := context.WithValue(context.Background(), conf.SkipHookKey, struct{}{})
	id, err := op.CreateStorage(ctx, model.Storage{Driver: fakedriver.DriverName, MountPath: mount, Addition: "{}", CacheExpiration: 30})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	s, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatal(err)
	}
	d := s.(*fakedriver.Driver)
	d.CloudLike = true
	return d
}

func readObject(t *testing.T, ctx context.Context, o fs.Object) (string, error) {
	t.Helper()
	rc, err := o.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

// Another node replaces a file with one of a different length; this node lists
// the folder again and opens the file again, and reads the new content, not what
// the link made for the old file still serves.
func TestReadAfterAnotherNodeReplacedTheFile(t *testing.T) {
	ctx := context.WithValue(context.Background(), conf.SkipHookKey, struct{}{})
	d := mountCloudLike(t, "/replaced")
	d.AddDir("/vol")
	d.AddFile("/vol/state.db", "the first version")
	olFs, err := rclonefs.NewFs(ctx, "/replaced/vol")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := olFs.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	o, err := olFs.NewObject(ctx, "state.db")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := readObject(t, ctx, o); err != nil || got != "the first version" {
		t.Fatalf("first read: %q, %v", got, err)
	}

	d.AddFile("/vol/state.db", "the second, longer version") // another node
	if _, err := olFs.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	o, err = olFs.NewObject(ctx, "state.db")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := readObject(t, ctx, o); err != nil || got != "the second, longer version" {
		t.Fatalf("read after the replacement: %q, %v", got, err)
	}
}

// An object taken before another node replaced the file is not read as a prefix
// of the new file: its size is the old one, the file is not.
func TestAnObjectListedBeforeTheReplacementIsNotReadAsAPrefix(t *testing.T) {
	ctx := context.WithValue(context.Background(), conf.SkipHookKey, struct{}{})
	d := mountCloudLike(t, "/prefix")
	d.AddDir("/vol")
	d.AddFile("/vol/state.db", "short")
	olFs, err := rclonefs.NewFs(ctx, "/prefix/vol")
	if err != nil {
		t.Fatal(err)
	}
	o, err := olFs.NewObject(ctx, "state.db")
	if err != nil {
		t.Fatal(err)
	}

	d.AddFile("/vol/state.db", "a much longer replacement") // another node
	if _, err := olFs.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	got, err := readObject(t, ctx, o)
	if !errors.Is(err, rclonefs.ErrObjectChanged) {
		t.Fatalf("read the old object after the replacement: %q, %v; want ErrObjectChanged", got, err)
	}
}
