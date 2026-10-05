// Package rclonefs exposes an OpenList mount path as an rclone backend, so
// rclone's sync, copy, bisync and check engines can run against any storage
// OpenList has mounted — in-process, with OpenList's own credentials, caches
// and per-storage rate limiters.
//
// Everything goes through internal/fs and internal/op by mount path; no
// driver is touched. The one place that needs care is Put: see NewPutStream.
package rclonefs

import (
	"context"
	"fmt"
	"io"
	stdpath "path"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	olfs "github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/pkg/errors"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
)

// Fs is an OpenList mount path seen as an rclone remote.
type Fs struct {
	root     string
	features *fs.Features
}

var (
	_ fs.Fs     = (*Fs)(nil)
	_ fs.Object = (*Object)(nil)
)

// NewFs returns an rclone Fs rooted at the OpenList mount path root. The root
// does not have to exist yet; rclone creates it on the first write.
func NewFs(ctx context.Context, root string) (*Fs, error) {
	f := &Fs{root: utils.FixAndCleanPath(root)}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)
	return f, nil
}

func (f *Fs) Name() string           { return "openlist" }
func (f *Fs) Root() string           { return f.root }
func (f *Fs) String() string         { return "openlist:" + f.root }
func (f *Fs) Features() *fs.Features { return f.features }
func (f *Fs) Hashes() hash.Set       { return hash.NewHashSet(hash.SHA1, hash.MD5) }
func (f *Fs) fullPath(remote string) string {
	return stdpath.Join(f.root, remote)
}

// Precision reports that modification times can't be set. Cloud drives like
// 115 keep their own upload time, so comparing mtimes would re-upload every
// file on every run; with this rclone compares size and hash instead.
func (f *Fs) Precision() time.Duration { return fs.ModTimeNotSupported }

// isNotFound covers both a missing object and a path no storage is mounted
// under — to rclone, either is "not there".
func isNotFound(err error) bool {
	return errs.IsObjectNotFound(err) || errors.Is(err, errs.StorageNotFound)
}

func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	full := f.fullPath(dir)
	if dir != "" {
		obj, err := olfs.Get(ctx, full, &olfs.GetArgs{NoLog: true})
		if err != nil {
			if isNotFound(err) {
				return nil, fs.ErrorDirNotFound
			}
			return nil, err
		}
		if !obj.IsDir() {
			return nil, fs.ErrorIsFile
		}
	}
	objs, err := olfs.List(ctx, full, &olfs.ListArgs{NoLog: true})
	if err != nil {
		if isNotFound(err) {
			return nil, fs.ErrorDirNotFound
		}
		return nil, err
	}
	entries := make(fs.DirEntries, 0, len(objs))
	for _, obj := range objs {
		remote := stdpath.Join(dir, obj.GetName())
		if obj.IsDir() {
			entries = append(entries, fs.NewDir(remote, obj.ModTime()))
			continue
		}
		entries = append(entries, f.newObject(remote, obj))
	}
	return entries, nil
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	obj, err := olfs.Get(ctx, f.fullPath(remote), &olfs.GetArgs{NoLog: true})
	if err != nil {
		if isNotFound(err) {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}
	if obj.IsDir() {
		return nil, fs.ErrorIsDir
	}
	return f.newObject(remote, obj), nil
}

func (f *Fs) newObject(remote string, obj model.Obj) *Object {
	return &Object{
		fs:      f,
		remote:  remote,
		size:    obj.GetSize(),
		modTime: obj.ModTime(),
		hashes:  obj.GetHash(),
	}
}

func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	obj, read, err := f.put(ctx, in, src)
	observeUpload(ctx, Upload{Remote: remote, Size: src.Size(), SourceRead: read, Err: err})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func (f *Fs) put(ctx context.Context, in io.Reader, src fs.ObjectInfo) (*Object, int64, error) {
	remote := src.Remote()
	full := f.fullPath(remote)
	dir, name := stdpath.Split(full)
	file, read, err := newPutStream(ctx, in, src, name)
	if err != nil {
		return nil, 0, err
	}
	storage, actualDir, err := op.GetStorageAndActualPath(dir)
	if err != nil {
		_ = file.Close()
		return nil, 0, errors.WithMessage(err, "failed get storage")
	}
	if storage.Config().NoUpload {
		_ = file.Close()
		return nil, 0, errors.WithStack(errs.UploadNotSupported)
	}
	// Remember what we already know about the content before op.Put closes
	// the stream: a driver that doesn't return the new object leaves only a
	// hashless placeholder in the dir cache.
	known := file.GetHash()
	if err := op.Put(ctx, storage, actualDir, file, nil); err != nil {
		return nil, read.Load(), err
	}
	o, err := f.NewObject(ctx, remote)
	if err != nil {
		return nil, read.Load(), fmt.Errorf("uploaded %s but can't find it: %w", full, err)
	}
	obj := o.(*Object)
	if obj.hashes.GetHash(utils.SHA1) == "" && obj.hashes.GetHash(utils.MD5) == "" {
		obj.hashes = known
	}
	return obj, read.Load(), nil
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	err := olfs.MakeDir(ctx, f.fullPath(dir))
	if err != nil && errs.IsObjectAlreadyExists(err) {
		return nil
	}
	return err
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	entries, err := f.List(ctx, dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fs.ErrorDirectoryNotEmpty
	}
	return olfs.Remove(ctx, f.fullPath(dir))
}
