package cfs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

// A volume is a reader until its first change. A second storage on the volume
// another one is writing to lists and reads it, and a change tells it plainly
// that another writer is writing.
func TestCfsStorageBesideAWriterReadsAndIsToldWhyItCannotWrite(t *testing.T) {
	ctx := context.Background()
	under := t.TempDir()
	addStorage(t, "Local", "/under-w1", `{"root_folder_path":`+q(under)+`}`)
	addStorage(t, "Local", "/under-w2", `{"root_folder_path":`+q(under)+`}`)
	add := func(tag string) string {
		return `{"remote_path":"/under-` + tag + `/vol","root_folder_path":"/","state_dir":` + q(t.TempDir()) +
			`,"seal_quiet":"50ms","seal_max":"500ms","threshold":"64KiB"}`
	}
	idA := addStorage(t, "Cfs", "/cfs-w1", add("w1"))
	idB := addStorage(t, "Cfs", "/cfs-w2", add("w2"))
	t.Cleanup(func() {
		op.DeleteStorageById(ctx, idA)
		op.DeleteStorageById(ctx, idB)
	})
	a, err := op.GetStorageByMountPath("/cfs-w1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := op.GetStorageByMountPath("/cfs-w2")
	if err != nil {
		t.Fatal(err)
	}
	if err := op.MakeDir(ctx, a, "/docs"); err != nil { // the first change: A takes the lease
		t.Fatal(err)
	}
	putFile(t, "/cfs-w1", "/docs", "a.txt", randBytes(500))
	if err := op.MakeDir(ctx, b, "/other"); err == nil || !strings.Contains(err.Error(), "another writer is writing") {
		t.Fatalf("B's change while A writes: %v", err)
	}
	if _, err := op.List(ctx, b, "/", model.ListArgs{}); err != nil {
		t.Fatalf("B lists while A writes: %v", err)
	}
}
