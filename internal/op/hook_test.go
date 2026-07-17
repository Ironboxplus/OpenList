package op_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type tokenValidStorageDriver struct {
	model.Storage
	addition struct{}
}

func (d *tokenValidStorageDriver) Config() driver.Config {
	return driver.Config{Name: "TokenValidTest", CheckStatus: true}
}

func (d *tokenValidStorageDriver) GetAddition() driver.Additional {
	return &d.addition
}

func (d *tokenValidStorageDriver) Init(ctx context.Context) error { return nil }

func (d *tokenValidStorageDriver) Drop(ctx context.Context) error { return nil }

func (d *tokenValidStorageDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	return nil, nil
}

func (d *tokenValidStorageDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return &model.Link{Header: http.Header{}}, nil
}

func TestNotifyStorageTokenValidRestoresStatus(t *testing.T) {
	storage := model.Storage{
		Driver:    "TokenValidTest",
		MountPath: "/token-valid-status",
		Status:    "old auth error",
		Addition:  "{}",
	}
	if err := db.CreateStorage(&storage); err != nil {
		t.Fatalf("CreateStorage failed: %v", err)
	}

	d := &tokenValidStorageDriver{Storage: storage}
	op.NotifyStorageTokenValid(d)

	if d.GetStorage().Status != op.WORK {
		t.Fatalf("in-memory status = %q, want %q", d.GetStorage().Status, op.WORK)
	}
	got, err := db.GetStorageById(storage.ID)
	if err != nil {
		t.Fatalf("GetStorageById failed: %v", err)
	}
	if got.Status != op.WORK {
		t.Fatalf("persisted status = %q, want %q", got.Status, op.WORK)
	}
}
func TestNotifyStorageTokenInvalidMarksStatusAndPersists(t *testing.T) {
	storage := model.Storage{
		Driver:    "TokenInvalidTest",
		MountPath: "/token-invalid-status",
		Status:    op.WORK,
		Addition:  "{}",
	}
	if err := db.CreateStorage(&storage); err != nil {
		t.Fatalf("CreateStorage failed: %v", err)
	}

	d := &tokenValidStorageDriver{Storage: storage}
	op.NotifyStorageTokenInvalid(d)

	if d.GetStorage().Status == op.WORK {
		t.Fatal("token-invalid must make the in-memory storage non-work before recovery hooks run")
	}
	got, err := db.GetStorageById(storage.ID)
	if err != nil {
		t.Fatalf("GetStorageById failed: %v", err)
	}
	if got.Status == op.WORK {
		t.Fatal("token-invalid must persist a non-work status before peer recovery")
	}
}

func TestNotifyStorageTokenHealthyDoesNotChangeLifecycleStatus(t *testing.T) {
	d := &tokenValidStorageDriver{Storage: model.Storage{Status: op.WORK}}
	called := make(chan struct{}, 1)
	op.RegisterStorageHealthHook(func(got driver.Driver) {
		if got != d {
			return
		}
		select {
		case called <- struct{}{}:
		default:
		}
	})

	op.NotifyStorageTokenHealthy(d)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("health hook was not invoked")
	}
	if d.GetStorage().Status != op.WORK {
		t.Fatal("a healthy proof must not mutate the storage lifecycle status")
	}
}
