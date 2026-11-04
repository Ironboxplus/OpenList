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

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
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
	// gone maps the full path of a directory this Fs removed or moved away to
	// the ID it had there, so that a listing which still shows it (a drive
	// that is slow to reflect a delete) isn't taken for the directory itself.
	gone sync.Map
	// observer, when set, is told about every finished Put on this Fs.
	observer func(Upload)
}

var (
	_ fs.Fs     = (*Fs)(nil)
	_ fs.Object = (*Object)(nil)
)

// NewFs returns an rclone Fs rooted at the OpenList mount path root. The root
// does not have to exist yet; rclone creates it on the first write.
func NewFs(ctx context.Context, root string, opts ...Option) (*Fs, error) {
	f := &Fs{root: utils.FixAndCleanPath(root)}
	for _, o := range opts {
		o(f)
	}
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
	// A directory this Fs already resolved needn't be looked up again: the
	// listing below fails by itself if it has since vanished. Listing the
	// same directory twice in a job (a pre-scan, then the transfer) then
	// costs one lookup, not two.
	if _, known := f.dirs.Load(utils.FixAndCleanPath(full)); dir != "" && !known {
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
			// Removed behind this Fs's back: nothing may be put in it now.
			f.forgetDir(full)
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
// its parent) carry no driver ID, so they can't be a Put parent; nor can a
// directory this Fs removed that a cache or the drive still shows.
func (f *Fs) rememberDir(full string, obj model.Obj) {
	if !obj.IsDir() || model.ObjHasMask(obj, model.Virtual) || f.isGone(full, obj) {
		return
	}
	f.dirs.Store(utils.FixAndCleanPath(full), model.UnwrapObjName(obj))
}

// buryDir records that dir, which forgetDir returned for full, is no longer
// there.
func (f *Fs) buryDir(full string, dir model.Obj) {
	if dir != nil && dir.GetID() != "" {
		f.gone.Store(utils.FixAndCleanPath(full), dir.GetID())
	}
}

// isGone reports whether obj, listed at full, is a directory this Fs removed
// or moved away from there.
func (f *Fs) isGone(full string, obj model.Obj) bool {
	id, ok := f.gone.Load(utils.FixAndCleanPath(full))
	return ok && obj.GetID() != "" && obj.GetID() == id.(string)
}

// forgetDir drops dir and everything remembered below it, and returns what
// was remembered for dir itself, if anything.
func (f *Fs) forgetDir(full string) model.Obj {
	full = utils.FixAndCleanPath(full)
	var dir model.Obj
	f.dirs.Range(func(k, v any) bool {
		if p := k.(string); p == full || strings.HasPrefix(p, strings.TrimSuffix(full, "/")+"/") {
			if p == full {
				dir = v.(model.Obj)
			}
			f.dirs.Delete(k)
		}
		return true
	})
	return dir
}

// Put is only called by rclone for a destination that does not exist, so it
// goes straight to the driver; Object.Update takes the checked path.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.upload(ctx, in, src, false)
}

func (f *Fs) upload(ctx context.Context, in io.Reader, src fs.ObjectInfo, exists bool) (fs.Object, error) {
	remote := src.Remote()
	obj, read, err := f.put(ctx, in, src, exists)
	if f.observer != nil {
		f.observer(Upload{Remote: remote, Size: src.Size(), SourceRead: read, Err: err})
	}
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
				// The directory may be what failed (removed behind this
				// Fs's back): the next Put resolves it afresh, and if the
				// driver said it is gone, not to the same one.
				f.forgetDir(dir)
				if errs.IsObjectNotFound(err) {
					f.buryDir(dir, parent)
				}
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
// from dirs when it is there. Transfers run in parallel and all miss at the
// start of a job, as does rclone's own Mkdir, so the miss path is serialised:
// the first caller resolves the directory, the rest find it in dirs.
//
// A miss is resolved the way rclone's lib/dircache does it: the parent comes
// from dirs (or is resolved the same way), one fresh listing of the parent
// says whether the directory is there, and if not it is made in the parent,
// whose object the driver needs anyway. A new directory costs a listing and a
// MakeDir; op.MakeDir would look the path and its parent up two or three
// times more, every one a rate-limit slot on drives like 115.
func (f *Fs) ensureDir(ctx context.Context, full string) (model.Obj, error) {
	key := utils.FixAndCleanPath(full)
	if v, ok := f.dirs.Load(key); ok {
		return v.(model.Obj), nil
	}
	f.dirMu.Lock()
	defer f.dirMu.Unlock()
	return f.ensureDirLocked(ctx, key)
}

func (f *Fs) ensureDirLocked(ctx context.Context, key string) (model.Obj, error) {
	if v, ok := f.dirs.Load(key); ok {
		return v.(model.Obj), nil
	}
	storage, actualDir, err := op.GetStorageAndActualPath(key)
	if err != nil {
		return nil, errors.WithMessage(err, "failed get storage")
	}
	obj, ok, err := f.makeInParent(ctx, storage, key, actualDir)
	if err != nil && (errors.Is(err, errs.NotFolder) || errors.Is(err, errs.PermissionDenied) || ctx.Err() != nil) {
		return nil, err
	}
	if !ok || err != nil {
		// The root of a storage, a parent on another storage, or something
		// the parent's listing or the driver objected to (a parent deleted
		// behind this Fs's back, a name another writer just took): resolve
		// the path the checked way, with nothing remembered below the parent.
		if err != nil {
			fs.Debugf(f, "resolve %s in its parent: %v; looking the path up instead", key, err)
		}
		if obj, err = f.makeChecked(ctx, storage, actualDir); err != nil {
			return nil, err
		}
	}
	f.rememberDir(key, obj)
	return obj, nil
}

// makeInParent finds or makes the directory at key in its parent. ok is false
// when key can't be resolved that way: it is the root of its storage, or its
// parent is on another storage (a mount point).
func (f *Fs) makeInParent(ctx context.Context, storage driver.Driver, key, actualDir string) (obj model.Obj, ok bool, err error) {
	if actualDir == "/" {
		return nil, false, nil
	}
	parentKey, name := stdpath.Dir(key), stdpath.Base(key)
	parentStorage, parentActual, err := op.GetStorageAndActualPath(parentKey)
	if err != nil || parentStorage.GetStorage() != storage.GetStorage() {
		return nil, false, nil
	}
	parent, err := f.ensureDirLocked(ctx, parentKey)
	if err != nil {
		return nil, true, err
	}
	// Fresh, not op's dir cache: a directory deleted since it was cached must
	// not be handed out as the parent of new files.
	objs, err := op.ListIn(ctx, storage, parent, parentActual, model.ListArgs{Refresh: true})
	if err != nil {
		return nil, true, f.parentFailed(parentKey, parent, err)
	}
	var file model.Obj
	for _, o := range objs {
		if o.GetName() != name || f.isGone(key, o) {
			continue
		}
		if o.IsDir() {
			return model.UnwrapObjName(o), true, nil
		}
		file = o
	}
	if file != nil {
		return nil, true, errors.WithStack(errs.NotFolder)
	}
	if obj, err = op.MakeDirIn(ctx, storage, parent, parentActual, name); err != nil {
		return nil, true, f.parentFailed(parentKey, parent, err)
	}
	// Made just now: whatever was removed from here before, this is not it,
	// even on a drive whose IDs are paths.
	f.gone.Delete(key)
	if obj == nil {
		// The driver doesn't report what it made.
		if obj, err = op.GetUnwrap(ctx, storage, actualDir); err != nil {
			return nil, true, err
		}
	}
	return obj, true, nil
}

// parentFailed is told that listing the remembered directory parent, or
// making a directory in it, failed with err, and returns err. Short of a name
// another writer took, the parent may have been removed behind this Fs's
// back, so it is forgotten, and if the driver says it is gone, its ID isn't
// taken from a listing again.
func (f *Fs) parentFailed(parentKey string, parent model.Obj, err error) error {
	if errs.IsObjectAlreadyExists(err) {
		return err
	}
	f.forgetDir(parentKey)
	if errs.IsObjectNotFound(err) {
		f.buryDir(parentKey, parent)
	}
	return err
}

// makeChecked makes the directory with op.MakeDir, which looks the path and
// its parents up first, and returns it.
func (f *Fs) makeChecked(ctx context.Context, storage driver.Driver, actualDir string) (model.Obj, error) {
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
	full := f.fullPath(dir)
	known := f.forgetDir(full)
	if err := olfs.Remove(ctx, full); err != nil {
		return err
	}
	f.buryDir(full, known)
	return nil
}
