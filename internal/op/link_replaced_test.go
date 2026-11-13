package op_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/OpenListTeam/OpenList/v4/drivers/local"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

// A storage that has no listing cache of its own (a local directory: Get asks
// the disk every time) hands out the file as it is now: while a link is open and
// cached, the file is replaced with one of another size, and the next Link
// returns the new file's object, not the one the cached link was made for.
func TestLinkOfAFileReplacedOnDiskIsNotTheCachedOne(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "state.db")
	if err := os.WriteFile(file, []byte("the first version"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := op.CreateStorage(ctx, model.Storage{
		Driver: "Local", MountPath: "/linkreplaced",
		Addition: `{"root_folder_path":"` + strings.ReplaceAll(dir, `\`, `\\`) + `"}`,
	})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	s, err := op.GetStorageByMountPath("/linkreplaced")
	if err != nil {
		t.Fatal(err)
	}

	// The link stays open, so it stays in the link cache.
	link1, obj1, err := op.Link(ctx, s, "/state.db", model.LinkArgs{})
	if err != nil {
		t.Fatal(err)
	}
	defer link1.Close()
	if obj1.GetSize() != int64(len("the first version")) {
		t.Fatalf("first link: size %d", obj1.GetSize())
	}

	if err := os.WriteFile(file, []byte("the second, longer version"), 0o644); err != nil {
		t.Fatal(err)
	}
	link2, obj2, err := op.Link(ctx, s, "/state.db", model.LinkArgs{})
	if err != nil {
		t.Fatal(err)
	}
	defer link2.Close()
	if want := int64(len("the second, longer version")); obj2.GetSize() != want {
		t.Fatalf("link after the replacement is for a file of %d bytes, want %d", obj2.GetSize(), want)
	}
}
