package cfs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cfsdrv "github.com/OpenListTeam/OpenList/v4/drivers/cfs"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

// The administrator's "maintain": a checkpoint of the volume, the lease handed
// back; anyone else is refused.
func TestCfsMaintainIsForAdministratorsAndCheckpointsTheVolume(t *testing.T) {
	under := t.TempDir()
	addStorage(t, "Local", "/under-mt", `{"root_folder_path":`+q(under)+`}`)
	add := `{"remote_path":"/under-mt/vol","root_folder_path":"/","state_dir":` + q(t.TempDir()) +
		`,"seal_quiet":"50ms","seal_max":"500ms","threshold":"64KiB"}`
	id := addStorage(t, "Cfs", "/cfs-mt", add)
	t.Cleanup(func() { op.DeleteStorageById(context.Background(), id) })
	st, err := op.GetStorageByMountPath("/cfs-mt")
	if err != nil {
		t.Fatal(err)
	}
	if err := op.MakeDir(context.Background(), st, "/docs"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		putFile(t, "/cfs-mt", "/docs", "f"+string(rune('a'+i))+".txt", randBytes(300+i))
	}
	d := st.(*cfsdrv.Cfs)
	as := func(role int) context.Context {
		return context.WithValue(context.Background(), conf.UserKey, &model.User{Role: role})
	}
	if _, err := d.Other(as(model.GENERAL), model.OtherArgs{Method: "maintain"}); err == nil || !strings.Contains(err.Error(), "administrators") {
		t.Fatalf("a general user's maintain: %v", err)
	}
	res, err := d.Other(as(model.ADMIN), model.OtherArgs{Method: "maintain"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	t.Logf("maintain: %v", m)
	if seq, _ := m["checkpoint_seq"].(uint64); seq == 0 {
		t.Errorf("checkpoint_seq in %v", m)
	}
	if w, _ := m["checkpoint_written"].(bool); !w {
		t.Errorf("checkpoint_written in %v", m)
	}
	cks, _ := os.ReadDir(filepath.Join(under, "vol", ".cfs", "ckpt"))
	if len(cks) < 1 {
		t.Errorf("no checkpoint on the drive after maintain")
	}
	lease, err := os.ReadFile(filepath.Join(under, "vol", ".cfs", "lease.json"))
	if err != nil || !strings.Contains(string(lease), `"released":true`) {
		t.Errorf("the lease after maintain: %s %v", lease, err)
	}
}
