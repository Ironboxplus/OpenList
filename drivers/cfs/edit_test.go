package cfs_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ironboxplus/cfs/core"
	cfsrclone "github.com/Ironboxplus/cfs/frontend/rclone"
	cfsdrv "github.com/OpenListTeam/OpenList/v4/drivers/cfs"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

// try calls f and turns a panic into an error.
func try(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return f()
}

func newEditableCfs(t *testing.T, tag string) (id uint, mount string, add func(threshold string) string) {
	t.Helper()
	under, state := t.TempDir(), t.TempDir()
	addStorage(t, "Local", "/under-"+tag, `{"root_folder_path":`+q(under)+`}`)
	add = func(threshold string) string {
		return `{"remote_path":"/under-` + tag + `/vol","root_folder_path":"/","state_dir":` + q(state) +
			`,"seal_quiet":"50ms","seal_max":"500ms","threshold":"` + threshold + `"}`
	}
	mount = "/cfs-" + tag
	id = addStorage(t, "Cfs", mount, add("64KiB"))
	t.Cleanup(func() { op.DeleteStorageById(context.Background(), id) })
	return
}

func editStorage(t *testing.T, id uint, addition string) {
	t.Helper()
	s, err := db.GetStorageById(id)
	if err != nil {
		t.Fatal(err)
	}
	s.Addition = addition
	if err := op.UpdateStorage(context.Background(), *s); err != nil {
		t.Fatalf("update storage: %v", err)
	}
}

// Every method of a driver instance that was dropped must fail with an error,
// not dereference the volume it no longer has.
func TestCfsDroppedInstanceReturnsErrors(t *testing.T) {
	ctx := context.Background()
	_, mount, _ := newEditableCfs(t, "dropped")
	st, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatal(err)
	}
	old := st.(*cfsdrv.Cfs)
	if err := old.Drop(ctx); err != nil {
		t.Fatal(err)
	}
	dir := &model.Object{Path: "/", IsFolder: true}
	file := &model.Object{Path: "/a.txt", Name: "a.txt"}
	calls := map[string]func() error{
		"List":    func() error { _, err := old.List(ctx, dir, model.ListArgs{}); return err },
		"Get":     func() error { _, err := old.Get(ctx, "/a.txt"); return err },
		"Link":    func() error { _, err := old.Link(ctx, file, model.LinkArgs{}); return err },
		"MakeDir": func() error { return old.MakeDir(ctx, dir, "d") },
		"Move":    func() error { return old.Move(ctx, file, dir) },
		"Rename":  func() error { return old.Rename(ctx, file, "b.txt") },
		"Remove":  func() error { return old.Remove(ctx, file) },
	}
	for name, f := range calls {
		if err := try(f); !errors.Is(err, errs.StorageNotInit) {
			t.Errorf("%s on a dropped instance: want StorageNotInit, got %v", name, err)
		}
	}
}

// Editing a storage drops the driver instance and initialises it again at the
// same mount path, while requests still use the old instance.
func TestCfsEditWhileListing(t *testing.T) {
	ctx := context.Background()
	id, mount, add := newEditableCfs(t, "edit")
	st, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatal(err)
	}
	old := st.(*cfsdrv.Cfs)
	if err := op.MakeDir(ctx, st, "/docs"); err != nil {
		t.Fatal(err)
	}
	putFile(t, mount, "/docs", "a.txt", randBytes(100))

	var stop atomic.Bool
	var wg sync.WaitGroup
	var failures, served atomic.Int64
	var firstBad atomic.Value
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				err := try(func() error {
					_, err := old.List(ctx, &model.Object{Path: "/docs", IsFolder: true}, model.ListArgs{})
					return err
				})
				switch {
				case err == nil:
					served.Add(1)
				case strings.HasPrefix(err.Error(), "panic"):
					failures.Add(1)
					firstBad.CompareAndSwap(nil, err)
				}
			}
		}()
	}
	for i := 0; i < 5; i++ {
		editStorage(t, id, add([]string{"32KiB", "64KiB"}[i%2]))
		time.Sleep(20 * time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()
	if failures.Load() > 0 {
		t.Fatalf("%d calls on the old instance panicked, first: %v", failures.Load(), firstBad.Load())
	}
	t.Logf("old instance served %d lists", served.Load())

	// The old instance now serves from the current volume.
	objs, err := old.List(ctx, &model.Object{Path: "/docs", IsFolder: true}, model.ListArgs{})
	if err != nil || len(objs) != 1 {
		t.Fatalf("list through the old instance after the edit: %d objects, err %v", len(objs), err)
	}
}

// A request that took the volume just before Drop closed it must get an error
// from the closed volume, not a panic.
func TestCfsClosedVolumeReturnsErrors(t *testing.T) {
	ctx := context.Background()
	_, mount, _ := newEditableCfs(t, "closed")
	st, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, mount, "/", "a.txt", randBytes(100))
	sh := cfsrclone.Lookup(cfsdrv.ShareKey(mount))
	if sh == nil {
		t.Fatal("volume not registered")
	}
	vol := sh.Volume()
	sh.Release(ctx)
	if err := st.Drop(ctx); err != nil {
		t.Fatal(err)
	}
	calls := map[string]func() error{
		"List":   func() error { _, err := vol.List(ctx, ""); return err },
		"Stat":   func() error { _, err := vol.Stat(ctx, "a.txt"); return err },
		"Open":   func() error { _, err := vol.Open(ctx, "a.txt", 0, -1); return err },
		"Mkdir":  func() error { return vol.Mkdir(ctx, "d") },
		"Move":   func() error { return vol.Move(ctx, "a.txt", "b.txt") },
		"Remove": func() error { return vol.Remove(ctx, "a.txt") },
		"Put": func() error {
			_, err := vol.PutWith(ctx, "c.txt", strings.NewReader("x"), core.PutOpts{Size: 1})
			return err
		},
	}
	for name, f := range calls {
		err := try(f)
		if err != nil && strings.HasPrefix(err.Error(), "panic") {
			t.Errorf("%s on a closed volume: %v", name, err)
		} else {
			t.Logf("%s on a closed volume: %v", name, err)
		}
	}
}
