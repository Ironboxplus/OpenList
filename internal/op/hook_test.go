package op_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	open115 "github.com/OpenListTeam/OpenList/v4/drivers/115_open"
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

func TestNotifyStorageTokenValidPublishesProvenPairWhileAlreadyWork(t *testing.T) {
	d := &open115.Open115{Storage: model.Storage{
		Driver:    "115 Open",
		MountPath: "/token-valid-rotated-pair",
		Status:    op.WORK,
		Addition:  `{"access_token":"fresh","refresh_token":"rotated"}`,
	}}
	proof := make(chan op.StorageCredentialEvent, 1)
	op.RegisterStorageCredentialHook(func(typ string, event op.StorageCredentialEvent) {
		if typ != "token-valid" || event.Storage != d {
			return
		}
		select {
		case proof <- event:
		default:
		}
	})

	op.NotifyStorageTokenValidWithSnapshot(d, d.Storage.Addition, d.Storage.Modified)

	select {
	case event := <-proof:
		if event.Addition != d.Storage.Addition || !event.Modified.Equal(d.Storage.Modified) {
			t.Fatalf("credential proof used the wrong generation: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("a proven rotated pair was not published while status was already WORK")
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

// An old client's 401 must be ignored after cluster recovery has installed a
// different pair. This uses the real 115 driver storage type plus real SQLite
// persistence; no test driver or callback mock is involved.
func TestNotifyStorageTokenInvalidWithStaleAdditionKeepsReplacementWorking(t *testing.T) {
	newAddition := `{"access_token":"new","refresh_token":"new"}`
	storage := model.Storage{
		Driver:    "115 Open",
		MountPath: "/token-invalid-stale-addition",
		Status:    op.WORK,
		Addition:  newAddition,
	}
	if err := db.CreateStorage(&storage); err != nil {
		t.Fatalf("CreateStorage failed: %v", err)
	}
	d := &open115.Open115{Storage: storage}
	op.NotifyStorageTokenInvalidWithAddition(d, `{"access_token":"old","refresh_token":"old"}`)
	if d.GetStorage().Status != op.WORK {
		t.Fatalf("stale invalidation changed in-memory status to %q", d.GetStorage().Status)
	}
	got, err := db.GetStorageById(storage.ID)
	if err != nil {
		t.Fatalf("GetStorageById failed: %v", err)
	}
	if got.Status != op.WORK {
		t.Fatalf("stale invalidation changed persisted status to %q", got.Status)
	}
}

// Addition equality alone is not enough: UpdateStorage can replace a row while
// an old client's callback is racing. The durable Modified generation is the
// compare-and-swap guard that prevents that callback from touching the new row.
func TestNotifyStorageTokenInvalidWithStaleGenerationKeepsReplacementWorking(t *testing.T) {
	oldAddition := `{"access_token":"old","refresh_token":"old"}`
	storage := model.Storage{
		Driver:    "115 Open",
		MountPath: "/token-invalid-stale-generation",
		Status:    op.WORK,
		Addition:  oldAddition,
		Modified:  time.Now().Add(-time.Minute),
	}
	if err := db.CreateStorage(&storage); err != nil {
		t.Fatalf("CreateStorage failed: %v", err)
	}
	oldGeneration := storage.Modified
	newStorage := storage
	newStorage.Addition = `{"access_token":"new","refresh_token":"new"}`
	newStorage.Modified = time.Now()
	if err := db.UpdateStorage(&newStorage); err != nil {
		t.Fatalf("UpdateStorage replacement failed: %v", err)
	}

	// d deliberately models the old client object whose request began before
	// the durable replacement. It is a real 115 driver value, not a fake.
	d := &open115.Open115{Storage: storage}
	op.NotifyStorageTokenInvalidWithSnapshot(d, oldAddition, oldGeneration)
	if d.GetStorage().Status != op.WORK {
		t.Fatalf("stale generation changed old in-memory status to %q", d.GetStorage().Status)
	}
	got, err := db.GetStorageById(storage.ID)
	if err != nil {
		t.Fatalf("GetStorageById failed: %v", err)
	}
	if got.Addition != newStorage.Addition || got.Status != op.WORK {
		t.Fatalf("stale generation changed replacement row: %#v", got)
	}
}

func TestNotifyStorageTokenInvalidDispatchesRecoveryWhenAlreadyInvalid(t *testing.T) {
	d := &tokenValidStorageDriver{Storage: model.Storage{
		Driver:    "TokenInvalidTest",
		MountPath: "/token-invalid-repeat",
		Status:    "token invalid",
		Addition:  "{}",
	}}
	called := make(chan struct{}, 1)
	op.RegisterStorageHook(func(typ string, got driver.Driver) {
		if typ != "token-invalid" || got != d {
			return
		}
		select {
		case called <- struct{}{}:
		default:
		}
	})

	op.NotifyStorageTokenInvalid(d)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("an already-invalid storage must still dispatch recovery")
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
