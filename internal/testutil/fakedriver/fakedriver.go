// Package fakedriver is an in-memory storage driver for tests that need to
// count how often op/fs layers call into a driver. It implements Getter,
// List, MakeDir and Put, and counts each of them.
package fakedriver

import (
	"bytes"
	"context"
	"io"
	stdpath "path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// DriverName is the name the driver is registered under.
const DriverName = "FakeCounting"

// Driver keeps files in memory, keyed by driver-side path.
type Driver struct {
	model.Storage
	addition struct{}

	mu    sync.Mutex
	nodes map[string]*model.Object
	data  map[string][]byte

	Gets, Lists, MakeDirs, Puts atomic.Int32

	// FailPut, when set, is asked before each Put with the driver-side path
	// of the file; a non-nil error fails the Put and stores nothing.
	FailPut func(path string) error

	// GetPaths records every path Get was asked for, in order.
	GetPaths []string
}

func init() {
	op.RegisterDriver(func() driver.Driver { return New() })
}

// New returns an empty driver whose root directory is "/".
func New() *Driver {
	return &Driver{nodes: map[string]*model.Object{}, data: map[string][]byte{}}
}

func (d *Driver) Config() driver.Config {
	return driver.Config{Name: DriverName, LocalSort: true, NoOverwriteUpload: false, DefaultRoot: "/"}
}
func (d *Driver) GetAddition() driver.Additional { return &d.addition }
func (d *Driver) Init(context.Context) error     { return nil }
func (d *Driver) Drop(context.Context) error     { return nil }
func (d *Driver) GetRootPath() string            { return "/" }

// ResetCounts zeroes the call counters.
func (d *Driver) ResetCounts() {
	d.Gets.Store(0)
	d.Lists.Store(0)
	d.MakeDirs.Store(0)
	d.Puts.Store(0)
	d.mu.Lock()
	d.GetPaths = nil
	d.mu.Unlock()
}

// Content returns what was stored at path, if anything.
func (d *Driver) Content(path string) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.data[path]
	return b, ok
}

// AddDir creates a directory (and no parents) without counting a call.
func (d *Driver) AddDir(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nodes[path] = d.newNode(path, true, 0)
}

// AddFile creates a file without counting a call.
func (d *Driver) AddFile(path, content string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nodes[path] = d.newNode(path, false, int64(len(content)))
	d.data[path] = []byte(content)
}

func (d *Driver) newNode(path string, dir bool, size int64) *model.Object {
	return &model.Object{
		ID:       path,
		Path:     path,
		Name:     stdpath.Base(path),
		Size:     size,
		Modified: time.Now(),
		IsFolder: dir,
		HashInfo: utils.NewHashInfo(nil, ""),
	}
}

func (d *Driver) Get(_ context.Context, path string) (model.Obj, error) {
	d.Gets.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.GetPaths = append(d.GetPaths, path)
	if n, ok := d.nodes[path]; ok {
		c := *n
		return &c, nil
	}
	return nil, errs.ObjectNotFound
}

func (d *Driver) List(_ context.Context, dir model.Obj, _ model.ListArgs) ([]model.Obj, error) {
	d.Lists.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []model.Obj
	for p, n := range d.nodes {
		if stdpath.Dir(p) == dir.GetPath() && p != "/" {
			c := *n
			out = append(out, &c)
		}
	}
	return out, nil
}

// Link serves the stored bytes of a file through a range reader.
func (d *Driver) Link(_ context.Context, file model.Obj, _ model.LinkArgs) (*model.Link, error) {
	b, ok := d.Content(file.GetPath())
	if !ok {
		return nil, errs.NotSupport
	}
	return &model.Link{
		ContentLength: int64(len(b)),
		RangeReader: stream.RangeReaderFunc(func(_ context.Context, r http_range.Range) (io.ReadCloser, error) {
			end := int64(len(b))
			if r.Length >= 0 && r.Start+r.Length < end {
				end = r.Start + r.Length
			}
			return io.NopCloser(bytes.NewReader(b[min(r.Start, end):end])), nil
		}),
	}, nil
}

func (d *Driver) MakeDir(_ context.Context, parent model.Obj, name string) (model.Obj, error) {
	d.MakeDirs.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	p := stdpath.Join(parent.GetPath(), name)
	n := d.newNode(p, true, 0)
	d.nodes[p] = n
	c := *n
	return &c, nil
}

func (d *Driver) Put(_ context.Context, parent model.Obj, file model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	d.Puts.Add(1)
	if d.FailPut != nil {
		if err := d.FailPut(stdpath.Join(parent.GetPath(), file.GetName())); err != nil {
			return nil, err
		}
	}
	b, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	p := stdpath.Join(parent.GetPath(), file.GetName())
	n := d.newNode(p, false, int64(len(b)))
	n.HashInfo = file.GetHash()
	d.nodes[p] = n
	d.data[p] = b
	up(100)
	c := *n
	return &c, nil
}

var (
	_ driver.Getter      = (*Driver)(nil)
	_ driver.MkdirResult = (*Driver)(nil)
	_ driver.PutResult   = (*Driver)(nil)
	_ driver.IRootPath   = (*Driver)(nil)
)

// Remove deletes a file or a directory with everything below it.
func (d *Driver) Remove(_ context.Context, obj model.Obj) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := obj.GetPath()
	for k := range d.nodes {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(d.nodes, k)
			delete(d.data, k)
		}
	}
	return nil
}

var _ driver.Remove = (*Driver)(nil)
