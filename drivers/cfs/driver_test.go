package cfs_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cfsdrv "github.com/OpenListTeam/OpenList/v4/drivers/cfs"
	_ "github.com/OpenListTeam/OpenList/v4/drivers/local"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	cfsrclone "github.com/Ironboxplus/cfs/frontend/rclone"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:cfsdrv_test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig(os.TempDir())
	db.Init(dB)
}

func q(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

func addStorage(t *testing.T, driver, mount, addition string) uint {
	t.Helper()
	id, err := op.CreateStorage(context.Background(), model.Storage{Driver: driver, MountPath: mount, Addition: addition})
	if err != nil {
		t.Fatalf("create %s storage: %v", driver, err)
	}
	return id
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func readAll(t *testing.T, mount, p string) []byte {
	t.Helper()
	ctx := context.Background()
	st, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatal(err)
	}
	link, _, err := op.Link(ctx, st, p, model.LinkArgs{})
	if err != nil {
		t.Fatalf("link %s: %v", p, err)
	}
	defer link.Close()
	rc, err := link.RangeReader.RangeRead(ctx, http_range.Range{Start: 0, Length: -1})
	if err != nil {
		t.Fatalf("range read %s: %v", p, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func putFile(t *testing.T, mount, dir, name string, data []byte) {
	t.Helper()
	ctx := context.Background()
	st, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatal(err)
	}
	fs := &stream.FileStream{
		Ctx:    ctx,
		Obj:    &model.Object{Name: name, Size: int64(len(data)), Modified: time.Now()},
		Reader: bytes.NewReader(data),
	}
	if err := op.Put(ctx, st, dir, fs, func(float64) {}); err != nil {
		t.Fatalf("put %s/%s: %v", dir, name, err)
	}
}

// The cfs storage on top of a Local storage: small files are packed, big ones
// are stored 1:1, and everything survives dropping and re-creating the storage.
func TestCfsStorageOnLocalStorage(t *testing.T) {
	ctx := context.Background()
	under, state := t.TempDir(), t.TempDir()
	addStorage(t, "Local", "/under", `{"root_folder_path":`+q(under)+`}`)
	cfsAdd := `{"remote_path":"/under/vol","root_folder_path":"/","state_dir":` + q(state) + `,"seal_quiet":"50ms","seal_max":"500ms","threshold":"64KiB"}`
	id := addStorage(t, "Cfs", "/cfs", cfsAdd)

	st, err := op.GetStorageByMountPath("/cfs")
	if err != nil {
		t.Fatal(err)
	}
	if err := op.MakeDir(ctx, st, "/docs"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	small, big := randBytes(3000), randBytes(200<<10)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			putFile(t, "/cfs", "/docs", "small"+string(rune('a'+i))+".txt", small[:100+i*10])
		}()
	}
	wg.Wait()
	putFile(t, "/cfs", "/docs", "big.bin", big)

	objs, err := op.List(ctx, st, "/docs", model.ListArgs{})
	if err != nil || len(objs) != 21 {
		t.Fatalf("list: %d objects, err %v", len(objs), err)
	}
	if got := readAll(t, "/cfs", "/docs/big.bin"); !bytes.Equal(got, big) {
		t.Fatal("big file differs")
	}
	if got := readAll(t, "/cfs", "/docs/smallc.txt"); !bytes.Equal(got, small[:120]) {
		t.Fatal("small file differs")
	}

	// Staged small files are acknowledged before they are in a pack: wait until
	// they have been committed (a real Flush of the volume the storage holds).
	sh0 := cfsrclone.Lookup(cfsdrv.ShareKey("/cfs"))
	if sh0 == nil {
		t.Fatal("the storage's volume is not registered")
	}
	if err := sh0.Volume().Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	sh0.Release(ctx)

	// What reached the underlying storage: packs + log, and the big file at its real path.
	if _, err := os.Stat(filepath.Join(under, "vol", "docs", "big.bin")); err != nil {
		t.Errorf("the big file must be stored 1:1: %v", err)
	}
	if _, err := os.Stat(filepath.Join(under, "vol", "docs", "smalla.txt")); err == nil {
		t.Errorf("a small file must live in a pack, not at its own path")
	}
	packs, _ := filepath.Glob(filepath.Join(under, "vol", ".cfs", "packs", "*.zip"))
	if len(packs) == 0 || len(packs) >= 20 {
		t.Errorf("expected a few packs for 20 small files, found %d", len(packs))
	}

	// Rename, move, remove.
	if err := op.Rename(ctx, st, "/docs/smallb.txt", "renamed.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := op.MakeDir(ctx, st, "/other"); err != nil {
		t.Fatal(err)
	}
	if err := op.Move(ctx, st, "/docs/renamed.txt", "/other"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := readAll(t, "/cfs", "/other/renamed.txt"); !bytes.Equal(got, small[:110]) {
		t.Fatal("moved file differs")
	}
	if err := op.Remove(ctx, st, "/docs/smalld.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := op.Move(ctx, st, "/other", "/docs"); err != nil {
		t.Fatalf("move dir: %v", err)
	}
	if got := readAll(t, "/cfs", "/docs/other/renamed.txt"); !bytes.Equal(got, small[:110]) {
		t.Fatal("file in moved directory differs")
	}

	// Both ways into the same volume: the process-wide registry holds it.
	sh := cfsrclone.Lookup(cfsdrv.ShareKey("/cfs"))
	if sh == nil {
		t.Fatal("the storage's volume is not registered for the other front ends")
	}
	if _, err := sh.Volume().Stat(ctx, "docs/big.bin"); err != nil {
		t.Errorf("stat through the shared volume: %v", err)
	}
	sh.Release(ctx)

	// Drop and create again: the volume reopens from its index and the drive.
	if err := op.DeleteStorageById(ctx, id); err != nil {
		t.Fatalf("delete storage: %v", err)
	}
	if cfsrclone.Lookup(cfsdrv.ShareKey("/cfs")) != nil {
		t.Fatal("the volume must be closed with the storage")
	}
	id = addStorage(t, "Cfs", "/cfs", cfsAdd)
	t.Cleanup(func() { op.DeleteStorageById(ctx, id) })
	st, _ = op.GetStorageByMountPath("/cfs")
	if got := readAll(t, "/cfs", "/docs/big.bin"); !bytes.Equal(got, big) {
		t.Fatal("big file differs after reopening")
	}
	if got := readAll(t, "/cfs", "/docs/other/renamed.txt"); !bytes.Equal(got, small[:110]) {
		t.Fatal("moved file differs after reopening")
	}
	if _, err := op.Get(ctx, st, "/docs/smalld.txt"); err == nil {
		t.Fatal("a removed file came back after reopening")
	}
	objs, _ = op.List(ctx, st, "/docs", model.ListArgs{})
	t.Logf("after reopening /docs holds %d entries", len(objs))
}

// A cfs storage must not be placed inside itself.
func TestCfsRejectsRemotePathInsideItself(t *testing.T) {
	_, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Cfs", MountPath: "/loop", Addition: `{"remote_path":"/loop/sub","root_folder_path":"/"}`,
	})
	if err == nil {
		t.Fatal("a cfs storage inside itself must be refused")
	}
}
