// Package fakedriver is an in-memory storage driver for tests that need to
// count how often op/fs layers call into a driver. It implements Getter,
// List, MakeDir and Put, and counts each of them.
package fakedriver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	stdpath "path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/net"
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

	// ListPaths records the driver-side path of every directory List was asked
	// for, in order.
	ListPaths []string

	// FailPut, when set, is asked before each Put with the driver-side path
	// of the file; a non-nil error fails the Put and stores nothing.
	FailPut func(path string) error

	// GetPaths records every path Get was asked for, in order.
	GetPaths []string

	// CloudLike makes the driver behave like a cloud drive rather than a
	// file system: every node gets an ID of its own, MakeDir refuses a name
	// that is taken, and nothing can be made or put in a directory that was
	// removed (ObjectNotFound). Set it before the first call.
	CloudLike bool
	// LagDeletes, with CloudLike, keeps a removed directory in its parent's
	// listing, as a drive slow to reflect a delete does; Get doesn't find it.
	LagDeletes bool
	// PathIDs, with CloudLike, gives each node its path as ID, as drives do
	// whose IDs are paths: a directory made again at a removed one's path has
	// the same ID.
	PathIDs bool
	// PutReportsNotFound makes Put return ObjectNotFound after the file is
	// stored, as drivers do that can't find the new file after the upload.
	PutReportsNotFound bool
	// OnMakeDir, when set, is called with the path of each MakeDir before it
	// is carried out (to let another writer take the name first).
	OnMakeDir func(path string)
	// OnRangeOpen, when set, is called each time Link's range reader opens a
	// stream, with the driver-side path of the file and the start of the range;
	// the function it returns is called once when that stream is closed.
	OnRangeOpen func(path string, start int64) (closed func())
	// PerFile, when above 0, makes the drive one that allows that many streams
	// open at once to a file: it declares the limit (driver.DownloadLimited) and
	// answers 403 to a range stream opened beyond it, as the 115 CDN does. A
	// stream is over when its body was read to the end, or closed.
	PerFile int
	// LinkConcurrency and LinkPartSize are the standard Link properties of the
	// links it gives out (0: not set).
	LinkConcurrency, LinkPartSize int
	// ReadDelay is how long reading 64 KiB of a stream takes.
	ReadDelay time.Duration
	seq       int
	ghosts    map[string]*model.Object

	omu     sync.Mutex
	open    map[string]int
	peak    map[string]int
	refused int
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
	d.ListPaths = nil
	d.mu.Unlock()
}

// ListCount returns how often the directory at path was listed since the last
// ResetCounts.
func (d *Driver) ListCount(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, p := range d.ListPaths {
		if p == path {
			n++
		}
	}
	return n
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
	id := path
	if d.CloudLike && !d.PathIDs {
		d.seq++
		id = fmt.Sprintf("%s#%d", path, d.seq)
	}
	return &model.Object{
		ID:       id,
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
	d.ListPaths = append(d.ListPaths, dir.GetPath())
	var out []model.Obj
	for _, nodes := range []map[string]*model.Object{d.nodes, d.ghosts} {
		for p, n := range nodes {
			if stdpath.Dir(p) == dir.GetPath() && p != "/" {
				c := *n
				out = append(out, &c)
			}
		}
	}
	return out, nil
}

// live reports, for CloudLike, whether parent is a directory that is still
// there (not one removed, nor another made at its path since). Call with mu.
func (d *Driver) live(parent model.Obj) bool {
	if !d.CloudLike || parent.GetPath() == "/" {
		return true
	}
	n, ok := d.nodes[parent.GetPath()]
	return ok && n.IsFolder && n.ID == parent.GetID()
}

// Link serves the stored bytes of a file through a range reader.
func (d *Driver) Link(_ context.Context, file model.Obj, _ model.LinkArgs) (*model.Link, error) {
	b, ok := d.Content(file.GetPath())
	if !ok {
		return nil, errs.NotSupport
	}
	path := file.GetPath()
	return &model.Link{
		ContentLength: int64(len(b)),
		Concurrency:   d.LinkConcurrency,
		PartSize:      d.LinkPartSize,
		RangeReader: stream.RangeReaderFunc(func(_ context.Context, r http_range.Range) (io.ReadCloser, error) {
			end := int64(len(b))
			if r.Length >= 0 && r.Start+r.Length < end {
				end = r.Start + r.Length
			}
			if !d.admit(path) {
				return nil, net.HttpStatusCodeError(http.StatusForbidden)
			}
			var rc io.ReadCloser = &counted{Reader: bytes.NewReader(b[min(r.Start, end):end]), d: d, path: path}
			if d.OnRangeOpen == nil {
				return rc, nil
			}
			return &notifyClose{ReadCloser: rc, closed: d.OnRangeOpen(path, r.Start)}, nil
		}),
	}, nil
}

// MaxDownloadConnectionsPerFile is PerFile.
func (d *Driver) MaxDownloadConnectionsPerFile() int { return d.PerFile }

// admit counts a stream opened to path, unless the drive allows no more.
func (d *Driver) admit(path string) bool {
	d.omu.Lock()
	defer d.omu.Unlock()
	if d.open == nil {
		d.open, d.peak = map[string]int{}, map[string]int{}
	}
	if d.PerFile > 0 && d.open[path] >= d.PerFile {
		d.refused++
		return false
	}
	d.open[path]++
	d.peak[path] = max(d.peak[path], d.open[path])
	return true
}

func (d *Driver) finish(path string) {
	d.omu.Lock()
	defer d.omu.Unlock()
	d.open[path]--
}

// PeakOpen is the most streams that were open to the file at once, and Refused
// how many were turned away for being beyond PerFile.
func (d *Driver) PeakOpen(path string) int {
	d.omu.Lock()
	defer d.omu.Unlock()
	return d.peak[path]
}

func (d *Driver) Refused() int {
	d.omu.Lock()
	defer d.omu.Unlock()
	return d.refused
}

// counted is a stream the drive counts as open until it is read to the end or
// closed.
type counted struct {
	io.Reader
	d    *Driver
	path string
	once sync.Once
}

func (c *counted) Read(p []byte) (int, error) {
	if c.d.ReadDelay > 0 {
		time.Sleep(c.d.ReadDelay * time.Duration(len(p)) / (64 << 10))
	}
	n, err := c.Reader.Read(p)
	if err == io.EOF {
		c.once.Do(func() { c.d.finish(c.path) })
	}
	return n, err
}

func (c *counted) Close() error {
	c.once.Do(func() { c.d.finish(c.path) })
	return nil
}

// notifyClose calls closed the first time it is closed.
type notifyClose struct {
	io.ReadCloser
	once   sync.Once
	closed func()
}

func (n *notifyClose) Close() error {
	n.once.Do(n.closed)
	return n.ReadCloser.Close()
}

func (d *Driver) MakeDir(_ context.Context, parent model.Obj, name string) (model.Obj, error) {
	d.MakeDirs.Add(1)
	p := stdpath.Join(parent.GetPath(), name)
	if d.OnMakeDir != nil {
		d.OnMakeDir(p)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.live(parent) {
		return nil, errs.ObjectNotFound
	}
	if _, taken := d.nodes[p]; taken && d.CloudLike {
		return nil, errs.ObjectAlreadyExists
	}
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
	if !d.live(parent) {
		return nil, errs.ObjectNotFound
	}
	p := stdpath.Join(parent.GetPath(), file.GetName())
	n := d.newNode(p, false, int64(len(b)))
	n.HashInfo = file.GetHash()
	d.nodes[p] = n
	d.data[p] = b
	up(100)
	if d.PutReportsNotFound {
		return nil, errs.ObjectNotFound
	}
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
			if n := d.nodes[k]; d.LagDeletes && k == p && n.IsFolder {
				if d.ghosts == nil {
					d.ghosts = map[string]*model.Object{}
				}
				d.ghosts[k] = n
			}
			delete(d.nodes, k)
			delete(d.data, k)
		}
	}
	return nil
}

var _ driver.Remove = (*Driver)(nil)
