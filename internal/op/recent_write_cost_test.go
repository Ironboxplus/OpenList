package op_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
)

// staleDriver models a provider without a Getter whose listing is always
// empty (stale), and counts List calls: the case where op.Get falls through to
// listing the parent and then to the recent-write bridge.
type staleDriver struct {
	model.Storage
	addition struct{}
	lists    atomic.Int32
	mu       sync.Mutex
	puts     int
}

func (d *staleDriver) Config() driver.Config {
	return driver.Config{Name: "StaleCounting", LocalSort: true, DefaultRoot: "/"}
}
func (d *staleDriver) GetAddition() driver.Additional { return &d.addition }
func (d *staleDriver) Init(context.Context) error     { return nil }
func (d *staleDriver) Drop(context.Context) error     { return nil }
func (d *staleDriver) GetRootId() string              { return "root-id" }
func (d *staleDriver) List(context.Context, model.Obj, model.ListArgs) ([]model.Obj, error) {
	d.lists.Add(1)
	return nil, nil
}
func (d *staleDriver) Link(context.Context, model.Obj, model.LinkArgs) (*model.Link, error) {
	return nil, errs.NotSupport
}
func (d *staleDriver) Put(_ context.Context, parent model.Obj, file model.FileStreamer, _ driver.UpdateProgress) (model.Obj, error) {
	d.mu.Lock()
	d.puts++
	d.mu.Unlock()
	return &model.Object{ID: "id-" + file.GetName(), Name: file.GetName(), Size: file.GetSize(), HashInfo: file.GetHash()}, nil
}

var _ driver.PutResult = (*staleDriver)(nil)

func newStale(id uint) *staleDriver {
	d := &staleDriver{}
	d.ID = id
	d.MountPath = "/stale"
	d.CacheExpiration = 30
	return d
}

func TestEmptyRecentWriteBridgeCostsNothing(t *testing.T) {
	ctx := context.WithValue(context.Background(), conf.SkipHookKey, struct{}{})
	d := newStale(9001)

	// Nothing was ever written through this storage. A missing name costs the
	// listing of its parent that op.Get always did, and nothing on top: in
	// particular no extra resolution of the parent directory.
	if _, err := op.Get(ctx, d, "/missing.txt"); !errs.IsObjectNotFound(err) {
		t.Fatalf("Get of a missing name: %v", err)
	}
	if got := d.lists.Load(); got != 1 {
		t.Fatalf("a miss with an empty bridge made %d List calls, want 1 (the parent listing)", got)
	}

	// With the bridge empty and the listing not cached (the stale driver lists
	// nothing, so op does not cache it), Remove of a missing name and a Rename
	// source lookup add nothing either.
	d.lists.Store(0)
	if err := op.Remove(ctx, d, "/missing.txt"); err != nil {
		t.Fatalf("Remove of a missing name: %v", err)
	}
	if got := d.lists.Load(); got != 1 {
		t.Fatalf("Remove of a missing name made %d List calls, want 1", got)
	}
}

func TestRecentWriteBridgeStillCoversAStaleListing(t *testing.T) {
	ctx := context.WithValue(context.Background(), conf.SkipHookKey, struct{}{})
	d := newStale(9002)
	root, err := op.GetUnwrap(ctx, d, "/")
	if err != nil {
		t.Fatal(err)
	}
	file := &stream.FileStream{Obj: &model.Object{Name: "new.txt", Size: 3}, Reader: strings.NewReader("abc")}
	if err := op.PutInDir(ctx, d, root, "/", file, nil); err != nil {
		t.Fatalf("PutInDir: %v", err)
	}

	// The backend still lists nothing, yet the file just written is found.
	obj, err := op.Get(ctx, d, "/new.txt")
	if err != nil {
		t.Fatalf("Get right after Put: %v", err)
	}
	if obj.GetID() != "id-new.txt" {
		t.Fatalf("Get returned %q", obj.GetID())
	}
	// Other names are still missing.
	if _, err := op.Get(ctx, d, "/other.txt"); !errs.IsObjectNotFound(err) {
		t.Fatalf("Get of another name: %v", err)
	}
}
