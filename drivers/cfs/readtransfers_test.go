package cfs_test

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	cfsdrv "github.com/OpenListTeam/OpenList/v4/drivers/cfs"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/testutil/fakedriver"
	cfsrclone "github.com/Ironboxplus/cfs/frontend/rclone"
)

// A drive such as 115 allows one connection per file, so two small files of the
// same pack must not be read at once. read_transfers bounds the open ranged
// reads apart from transfers (the uploads), which stay at 8 here.
func TestCfsReadTransfersBoundsRangedReads(t *testing.T) {
	ctx := t.Context()
	underID := addStorage(t, fakedriver.DriverName, "/readtr-under", "{}")
	under, err := op.GetStorageByMountPath("/readtr-under")
	if err != nil {
		t.Fatal(err)
	}
	drive := under.(*fakedriver.Driver)
	drive.CloudLike = true
	drive.AddDir("/vol")
	cfsID := addStorage(t, "Cfs", "/readtr-cfs", `{"remote_path":"/readtr-under/vol","root_folder_path":"/","state_dir":`+q(t.TempDir())+`,"ack":"durable","transfers":8,"read_transfers":1}`)
	t.Cleanup(func() {
		op.DeleteStorageById(ctx, cfsID)
		op.DeleteStorageById(ctx, underID)
	})

	want := map[string][]byte{}
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("f%02d.txt", i)
		want[name] = randBytes(10000 + i)
		putFile(t, "/readtr-cfs", "/", name, want[name])
	}
	sh := cfsrclone.Lookup(cfsdrv.ShareKey("/readtr-cfs"))
	if sh == nil {
		t.Fatal("the storage's volume is not registered")
	}
	defer sh.Release(ctx)
	if err := sh.Volume().Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Count the bodies open on the drive over all its files; each open takes a
	// moment, so that reads that are allowed to overlap do overlap.
	var mu sync.Mutex
	open, peak, opened := 0, 0, 0
	drive.OnRangeOpen = func(string, int64) func() {
		mu.Lock()
		opened++
		open++
		peak = max(peak, open)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		return func() {
			mu.Lock()
			open--
			mu.Unlock()
		}
	}
	t.Cleanup(func() { drive.OnRangeOpen = nil })

	var wg sync.WaitGroup
	for name, w := range want {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := readAll(t, "/readtr-cfs", "/"+name); !bytes.Equal(got, w) {
				t.Errorf("%s differs", name)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d ranged reads opened on the drive, at most %d open at once (meter: %s)", opened, peak, sh.Meter().Snapshot())
	if opened == 0 {
		t.Fatal("no ranged read reached the drive")
	}
	if peak != 1 {
		t.Errorf("%d ranged reads were open at once with read_transfers=1", peak)
	}
}
