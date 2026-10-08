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
	"strings"
	"sync"
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
	// dirs maps a directory's full path to the object a driver needs as the
	// parent of a Put. It lives as long as the Fs (one job), unlike op's dir
	// cache which expires mid-job, and unlike op.Get which, on drivers with a
	// Getter, never caches the directory at all. Without it every uploaded
	// file costs two or three directory lookups on top of the upload.
	dirs  sync.Map
	dirMu sync.Mutex
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
		f.rememberDir(full, obj)
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

// rememberDir records dir for Put. Virtual objects (a mount point seen from
// its parent) carry no driver ID, so they can't be a Put parent.
func (f *Fs) rememberDir(full string, obj model.Obj) {
	if !obj.IsDir() || model.ObjHasMask(obj, model.Virtual) {
		return
	}
	f.dirs.Store(utils.FixAndCleanPath(full), model.UnwrapObjName(obj))
}

// forgetDir drops dir and everything remembered below it.
func (f *Fs) forgetDir(full string) {
	full = utils.FixAndCleanPath(full)
	f.dirs.Range(func(k, _ any) bool {
		if p := k.(string); p == full || strings.HasPrefix(p, strings.TrimSuffix(full, "/")+"/") {
			f.dirs.Delete(k)
		}
		return true
	})
}

// Put is only called by rclone for a destination that does not exist, so it
// goes straight to the driver; Object.Update takes the checked path.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.upload(ctx, in, src, false)
}

func (f *Fs) upload(ctx context.Context, in io.Reader, src fs.ObjectInfo, exists bool) (fs.Object, error) {
	remote := src.Remote()
	obj, read, err := f.put(ctx, in, src, exists)
	observeUpload(ctx, Upload{Remote: remote, Size: src.Size(), SourceRead: read, Err: err})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func (f *Fs) put(ctx context.Context, in io.Reader, src fs.ObjectInfo, exists bool) (*Object, int64, error) {
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
	// Remember what we already know about the content before the stream is
	// closed: a driver that doesn't return the new object leaves only a
	// hashless placeholder in the dir cache.
	known := file.GetHash()
	if !exists {
		if parent, err := f.ensureDir(ctx, dir); err == nil {
			defer func() {
				if err := file.Close(); err != nil {
					fs.Logf(f, "close upload stream: %v", err)
				}
			}()
			if err := op.PutInDir(ctx, storage, parent, actualDir, file, nil); err != nil {
				return nil, read.Load(), err
			}
			// Everything about the new object is already known; asking the
			// drive for it again would cost another lookup per file.
			return &Object{fs: f, remote: remote, size: src.Size(), modTime: src.ModTime(ctx), hashes: known}, read.Load(), nil
		}
	}
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

// ensureDir makes sure the directory at full exists and returns its object,
// from dirs when it is there. Otherwise it creates the directory if need be
// and looks it up once, then remembers it. Transfers run in parallel and all
// miss at the start of a job, as does rclone's own Mkdir, so the miss path is
// serialised: the first caller resolves the directory, the rest find it in
// dirs.
func (f *Fs) ensureDir(ctx context.Context, full string) (model.Obj, error) {
	key := utils.FixAndCleanPath(full)
	if v, ok := f.dirs.Load(key); ok {
		return v.(model.Obj), nil
	}
	f.dirMu.Lock()
	defer f.dirMu.Unlock()
	if v, ok := f.dirs.Load(key); ok {
		return v.(model.Obj), nil
	}
	storage, actualDir, err := op.GetStorageAndActualPath(key)
	if err != nil {
		return nil, errors.WithMessage(err, "failed get storage")
	}
	if err := op.MakeDir(ctx, storage, actualDir); err != nil && !errs.IsObjectAlreadyExists(err) {
		return nil, err
	}
	obj, err := op.GetUnwrap(ctx, storage, actualDir)
	if err != nil {
		return nil, err
	}
	if !obj.IsDir() {
		return nil, errors.WithStack(errs.NotFolder)
	}
	f.dirs.Store(key, obj)
	return obj, nil
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.ensureDir(ctx, f.fullPath(dir))
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
	f.forgetDir(f.fullPath(dir))
	return olfs.Remove(ctx, f.fullPath(dir))
}
