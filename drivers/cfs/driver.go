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

	"github.com/Ironboxplus/cfs/core"
	cfsrclone "github.com/Ironboxplus/cfs/frontend/rclone"
	"github.com/Ironboxplus/cfs/store"
	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/rclonefs"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
)

type Cfs struct {
	model.Storage
	Addition
	shared *cfsrclone.Shared
	vol    *core.Volume
}

func (d *Cfs) Config() driver.Config { return config }

func (d *Cfs) GetAddition() driver.Additional { return &d.Addition }

// ShareKey is the key the volume of the storage mounted at mountPath is
// registered under, for front ends in the same process (see cmd/cfs.go).
func ShareKey(mountPath string) string { return "openlist-storage:" + utils.FixAndCleanPath(mountPath) }

// volumes maps ShareKey to the volume of the storage currently mounted there.
var volumes sync.Map

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
		"dir_cache": d.DirCache, "gc_grace": d.GCGrace,
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
	sh, err := cfsrclone.Acquire(ctx, ShareKey(d.MountPath), func() (fs.Fs, error) {
		return rclonefs.NewFs(ctx, remote)
	}, opt)
	if err != nil {
		return err
	}
	d.shared, d.vol = sh, sh.Volume()
	volumes.Store(ShareKey(d.MountPath), d.vol)
	return nil
}

func (d *Cfs) Drop(ctx context.Context) error {
	if d.shared == nil {
		return nil
	}
	sh := d.shared
	volumes.CompareAndDelete(ShareKey(d.MountPath), d.vol)
	d.shared, d.vol = nil, nil
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
	es, err := d.vol.List(ctx, rel(dir.GetPath()))
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
	e, err := d.vol.Stat(ctx, r)
	if err != nil {
		return nil, mapErr(err)
	}
	return objOf(e), nil
}

func (d *Cfs) Link(ctx context.Context, file model.Obj, _ model.LinkArgs) (*model.Link, error) {
	p, key := rel(file.GetPath()), ShareKey(d.MountPath)
	// Fail now rather than on the first read.
	if _, err := d.vol.Stat(ctx, p); err != nil {
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
	return mapErr(d.vol.Mkdir(ctx, stdpath.Join(rel(parentDir.GetPath()), dirName)))
}

func (d *Cfs) moveTo(ctx context.Context, src model.Obj, dst string) error {
	if src.IsDir() {
		return mapErr(d.vol.DirMove(ctx, rel(src.GetPath()), dst))
	}
	return mapErr(d.vol.Move(ctx, rel(src.GetPath()), dst))
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
	if obj.IsDir() {
		return mapErr(d.vol.RemoveAll(ctx, rel(obj.GetPath())))
	}
	return mapErr(d.vol.Remove(ctx, rel(obj.GetPath())))
}

func (d *Cfs) Put(ctx context.Context, dstDir model.Obj, s model.FileStreamer, up driver.UpdateProgress) error {
	p := stdpath.Join(rel(dstDir.GetPath()), s.GetName())
	var r io.Reader
	if size := s.GetSize(); size >= d.vol.Config().Threshold {
		// A big file goes to the underlying storage 1:1. Cache it first, so that a
		// failed upload can be repeated without asking the client to send it again.
		f, err := s.CacheFullAndWriter(&up, nil)
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
	if _, err := d.vol.Put(ctx, p, r, s.GetSize(), s.ModTime()); err != nil {
		return mapErr(err)
	}
	up(100)
	return nil
}

var _ driver.Driver = (*Cfs)(nil)
