// Package cfs is the OpenList front end of the cfs filesystem (github.com/
// Ironboxplus/cfs): small files are packed into a few big uploads on another
// storage, big files are stored 1:1 there. The storage below is reached by
// mount path through OpenList itself, so its credentials, caches and rate
// limiter apply.
package cfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdpath "path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ironboxplus/cfs/core"
	cfsrclone "github.com/Ironboxplus/cfs/frontend/rclone"
	"github.com/Ironboxplus/cfs/store"
	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/rclonefs"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/shirou/gopsutil/v4/disk"
	log "github.com/sirupsen/logrus"
)

type Cfs struct {
	model.Storage
	Addition
	shared *cfsrclone.Shared
}

func (d *Cfs) Config() driver.Config { return config }

func (d *Cfs) GetAddition() driver.Additional { return &d.Addition }

// ShareKey is the key the volume of the storage mounted at mountPath is
// registered under, for front ends in the same process (see cmd/cfs.go).
func ShareKey(mountPath string) string { return "openlist-storage:" + utils.FixAndCleanPath(mountPath) }

// volumes maps ShareKey to the volume of the storage currently mounted there.
var volumes sync.Map

// volume returns the volume the mount path has now. OpenList keeps calling an
// old driver instance after the storage was edited (Drop, then Init of a new
// instance), so the instance itself holds no volume.
func (d *Cfs) volume() (*core.Volume, error) {
	v, ok := volumes.Load(ShareKey(d.MountPath))
	if !ok {
		return nil, errs.StorageNotInit
	}
	return v.(*core.Volume), nil
}

func (d *Cfs) Init(ctx context.Context) error {
	d.RemotePath = utils.FixAndCleanPath(d.RemotePath)
	mount := utils.FixAndCleanPath(d.MountPath)
	if d.RemotePath == "/" || d.RemotePath == mount || strings.HasPrefix(d.RemotePath+"/", mount+"/") {
		return fmt.Errorf("remote_path %q must be a folder of another storage, not %q itself", d.RemotePath, mount)
	}
	state := d.StateDir
	if state == "" {
		state = filepath.Join(flags.DataDir, "cfs", strconv.FormatUint(uint64(d.ID), 10))
	}
	m := configmap.Simple{"state_dir": state}
	for k, v := range map[string]string{
		"threshold": d.Threshold, "seal_quiet": d.SealQuiet, "seal_max": d.SealMax,
		"ack": d.Ack, "stage_max_bytes": d.StageMax, "pack_max_bytes": d.PackMax,
		"dir_cache": d.DirCache, "gc_grace": d.GCGrace, "lease_ttl": d.LeaseTTL,
	} {
		if v != "" {
			m[k] = v
		}
	}
	if d.MetaInflight > 0 {
		m["meta_inflight"] = strconv.Itoa(d.MetaInflight)
	}
	if d.Transfers > 0 {
		m["transfers"] = strconv.Itoa(d.Transfers)
	}
	opt, err := cfsrclone.OptionsFrom(m)
	if err != nil {
		return fmt.Errorf("cfs options: %w", err)
	}
	remote := d.RemotePath
	if err := waitBacking(ctx, remote); err != nil {
		return err
	}
	sh, err := cfsrclone.Acquire(ctx, ShareKey(d.MountPath), func() (fs.Fs, error) {
		return rclonefs.NewFs(ctx, remote)
	}, opt)
	if err != nil {
		return err
	}
	d.shared = sh
	volumes.Store(ShareKey(d.MountPath), sh.Volume())
	return nil
}

// backingWait is how long Init waits for the storage under remote_path.
const backingWait = 2 * time.Minute

// waitBacking returns once the storage that owns remote is loaded and working.
//
// Storages load concurrently, one goroutine per driver (bootstrap.LoadStorages),
// and OpenList has no ordering between them: Alias and Crypt only resolve their
// target per request, so a target that is not there yet costs them nothing.
// The volume of a cfs storage is opened in Init, though, so a backing storage
// that is not mounted yet (a 115 storage still refreshing its token, say) made
// Init fail for good with "storage not found". A storage is put in the map only
// when its Init returned, so being found there means it has finished loading, and its status is final.
// A backing storage that loaded with an error is reported at once instead of
// waiting for it. The wait ends with ctx and after backingWait.
func waitBacking(ctx context.Context, remote string) error {
	timeout := time.NewTimer(backingWait)
	defer timeout.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	waiting := false
	for {
		st, _, err := op.GetStorageAndActualPath(remote)
		if err == nil {
			if status := st.GetStorage().Status; status != op.WORK {
				return fmt.Errorf("the storage %s under remote_path %s is not working: %s", st.GetStorage().MountPath, remote, status)
			}
			return nil
		}
		if !errors.Is(err, errs.StorageNotFound) {
			return err
		}
		if !waiting {
			waiting = true
			log.Infof("[cfs] waiting for the storage that holds %s to load", remote)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("no storage holds remote_path %s after %s: %w", remote, backingWait, err)
		case <-tick.C:
		}
	}
}

func (d *Cfs) Drop(ctx context.Context) error {
	if d.shared == nil {
		return nil
	}
	sh := d.shared
	volumes.CompareAndDelete(ShareKey(d.MountPath), sh.Volume())
	d.shared = nil
	return sh.Release(ctx)
}

func (a Addition) GetRootPath() string { return a.RootFolderPath }

// rel turns an OpenList path into a volume path.
func rel(p string) string {
	return strings.Trim(stdpath.Clean("/"+p), "/")
}

func objOf(e core.Entry) model.Obj {
	o := &model.Object{
		Path:     "/" + e.Path,
		Name:     e.Name(),
		Size:     e.Size,
		Modified: e.ModTime,
		Ctime:    e.ModTime,
		IsFolder: e.IsDir,
	}
	if e.SHA1 != "" {
		o.HashInfo = utils.NewHashInfo(utils.SHA1, e.SHA1)
	}
	return o
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return errs.ObjectNotFound
	case errors.Is(err, store.ErrExists):
		return errs.ObjectAlreadyExists
	}
	return err
}

func (d *Cfs) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	vol, err := d.volume()
	if err != nil {
		return nil, err
	}
	es, err := vol.List(ctx, rel(dir.GetPath()))
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]model.Obj, 0, len(es))
	for _, e := range es {
		out = append(out, objOf(e))
	}
	return out, nil
}

func (d *Cfs) Get(ctx context.Context, path string) (model.Obj, error) {
	r := rel(path)
	if r == "" {
		return &model.Object{Path: "/", Name: "/", IsFolder: true}, nil
	}
	vol, err := d.volume()
	if err != nil {
		return nil, err
	}
	e, err := vol.Stat(ctx, r)
	if err != nil {
		return nil, mapErr(err)
	}
	return objOf(e), nil
}

func (d *Cfs) Link(ctx context.Context, file model.Obj, _ model.LinkArgs) (*model.Link, error) {
	p, key := rel(file.GetPath()), ShareKey(d.MountPath)
	vol, err := d.volume()
	if err != nil {
		return nil, err
	}
	// Fail now rather than on the first read.
	if _, err := vol.Stat(ctx, p); err != nil {
		return nil, mapErr(err)
	}
	return &model.Link{
		RangeReader: stream.RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
			// OpenList keeps links after the storage was edited (which makes a new
			// driver instance): read from whatever volume the mount path has now.
			vol, ok := volumes.Load(key)
			if !ok {
				return nil, errs.StorageNotInit
			}
			rc, err := vol.(*core.Volume).Open(ctx, p, r.Start, r.Length)
			return rc, mapErr(err)
		}),
	}, nil
}

func (d *Cfs) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	vol, err := d.volume()
	if err != nil {
		return err
	}
	return mapErr(vol.Mkdir(ctx, stdpath.Join(rel(parentDir.GetPath()), dirName)))
}

func (d *Cfs) moveTo(ctx context.Context, src model.Obj, dst string) error {
	vol, err := d.volume()
	if err != nil {
		return err
	}
	if src.IsDir() {
		return mapErr(vol.DirMove(ctx, rel(src.GetPath()), dst))
	}
	return mapErr(vol.Move(ctx, rel(src.GetPath()), dst))
}

func (d *Cfs) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	return d.moveTo(ctx, srcObj, stdpath.Join(rel(dstDir.GetPath()), srcObj.GetName()))
}

func (d *Cfs) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	return d.moveTo(ctx, srcObj, stdpath.Join(stdpath.Dir(rel(srcObj.GetPath())), newName))
}

// Copy is left to OpenList (read, then write): a server-side copy of a packed
// file is a new member, which only Put knows how to make.
func (d *Cfs) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NotImplement
}

func (d *Cfs) Remove(ctx context.Context, obj model.Obj) error {
	vol, err := d.volume()
	if err != nil {
		return err
	}
	if obj.IsDir() {
		return mapErr(vol.RemoveAll(ctx, rel(obj.GetPath())))
	}
	return mapErr(vol.Remove(ctx, rel(obj.GetPath())))
}

func (d *Cfs) Put(ctx context.Context, dstDir model.Obj, s model.FileStreamer, up driver.UpdateProgress) error {
	vol, err := d.volume()
	if err != nil {
		return err
	}
	p := stdpath.Join(rel(dstDir.GetPath()), s.GetName())
	opts := core.PutOpts{Size: s.GetSize(), MTime: s.ModTime(), SHA1: s.GetHash().GetHash(utils.SHA1)}
	var r io.Reader
	if opts.Size >= vol.Config().Threshold {
		// A big file goes to the underlying storage 1:1, and a failed upload is
		// repeated from the start of a seekable source.
		f, err := seekableSource(s, &up)
		if err != nil {
			return err
		}
		r = struct {
			io.Reader
			io.Seeker
		}{driver.NewLimitedUploadStream(ctx, f), f}
	} else {
		r = driver.NewLimitedUploadStream(ctx, &driver.ReaderUpdatingProgress{Reader: s, UpdateProgress: up})
	}
	if _, err := vol.PutWith(ctx, p, r, opts); err != nil {
		return mapErr(err)
	}
	up(100)
	return nil
}

// spareTemp is what must stay free on the disk of the temp dir after a file
// was cached there.
const spareTemp = 1 << 30

// seekableSource returns the content of s as something that can be read again
// from the start. A stream that is already seekable (backed by a file, or one
// that can be read by range from its source) is used as it is; only the others
// are cached, after checking that the temp dir can take the copy.
func seekableSource(s model.FileStreamer, up *driver.UpdateProgress) (io.ReadSeeker, error) {
	if f := s.GetFile(); f != nil {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return f, nil
	}
	if ss, ok := s.(*stream.SeekableStream); ok {
		return &rangeFile{s: ss, size: ss.GetSize()}, nil
	}
	if err := checkTempSpace(s.GetSize()); err != nil {
		return nil, err
	}
	return s.CacheFullAndWriter(up, nil)
}

// checkTempSpace refuses to cache size bytes when the disk of the temp dir
// would be left with less than spareTemp.
func checkTempSpace(size int64) error {
	u, err := disk.Usage(conf.Conf.TempDir)
	if err != nil {
		return fmt.Errorf("check free space of temp dir %s: %w", conf.Conf.TempDir, err)
	}
	if need := uint64(size) + spareTemp; u.Free < need {
		return fmt.Errorf("temp dir has %s free, need %s (file %s + %s to spare)",
			sizeStr(u.Free), sizeStr(need), sizeStr(uint64(size)), sizeStr(spareTemp))
	}
	return nil
}

func sizeStr(n uint64) string { return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30)) }

// rangeFile reads a stream by range from its source, as a ReadSeeker: nothing
// is cached, a Seek (a repeated upload starts with one) just starts a new range.
type rangeFile struct {
	s    *stream.SeekableStream
	size int64
	off  int64
	cur  io.Reader
}

func (f *rangeFile) Read(p []byte) (int, error) {
	if f.off >= f.size {
		return 0, io.EOF
	}
	if f.cur == nil {
		r, err := f.s.RangeRead(http_range.Range{Start: f.off, Length: -1})
		if err != nil {
			return 0, err
		}
		f.cur = r
	}
	n, err := f.cur.Read(p)
	f.off += int64(n)
	return n, err
}

func (f *rangeFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += f.off
	case io.SeekEnd:
		offset += f.size
	}
	if offset < 0 {
		return 0, errors.New("cfs: seek before the start of the file")
	}
	if offset != f.off {
		f.off, f.cur = offset, nil
	}
	return offset, nil
}

var _ driver.Driver = (*Cfs)(nil)
