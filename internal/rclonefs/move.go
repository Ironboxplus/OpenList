package rclonefs

import (
	"context"
	stdpath "path"

	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	olfs "github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/pkg/errors"
	"github.com/rclone/rclone/fs"
)

// Move moves a file server-side when source and destination are on the same
// storage. This is what makes --track-renames and bisync's rename handling
// one API call instead of a re-upload.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}
	if err := moveWithin(ctx, srcObj.fs.fullPath(srcObj.remote), f.fullPath(remote), fs.ErrorCantMove); err != nil {
		return nil, err
	}
	return f.NewObject(ctx, remote)
}

// DirMove moves a whole directory server-side, same storage only.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		return fs.ErrorCantDirMove
	}
	srcFull, dstFull := srcFs.fullPath(srcRemote), f.fullPath(dstRemote)
	moved := srcFs.forgetDir(srcFull)
	f.forgetDir(dstFull)
	if _, err := olfs.Get(ctx, dstFull, &olfs.GetArgs{NoLog: true}); err == nil {
		return fs.ErrorDirExists
	}
	if err := moveWithin(ctx, srcFull, dstFull, fs.ErrorCantDirMove); err != nil {
		return err
	}
	// The directory is at dstFull now, under the ID it had at srcFull.
	srcFs.buryDir(srcFull, moved)
	f.gone.Delete(utils.FixAndCleanPath(dstFull))
	return nil
}

// moveWithin is a driver Move into the destination's directory followed by a
// Rename when the name changes — the two primitives OpenList drivers have.
// Anything the storage can't do server-side comes back as cant, which tells
// rclone to fall back to copy + delete.
func moveWithin(ctx context.Context, srcFull, dstFull string, cant error) error {
	storage, srcActual, err := op.GetStorageAndActualPath(srcFull)
	if err != nil {
		return err
	}
	dstStorage, dstDirActual, err := op.GetStorageAndActualPath(stdpath.Dir(dstFull))
	if err != nil {
		return err
	}
	if storage.GetStorage() != dstStorage.GetStorage() {
		return cant
	}
	unsupported := func(err error) bool {
		return errors.Is(err, errs.NotImplement) || errors.Is(err, errs.NotSupport)
	}
	srcName, dstName := stdpath.Base(srcActual), stdpath.Base(dstFull)
	if stdpath.Dir(srcActual) != dstDirActual {
		if err := op.MakeDir(ctx, storage, dstDirActual); err != nil && !errs.IsObjectAlreadyExists(err) {
			return err
		}
		if err := op.Move(ctx, storage, srcActual, dstDirActual); err != nil {
			if unsupported(err) {
				return cant
			}
			return err
		}
		srcActual = stdpath.Join(dstDirActual, srcName)
	}
	if srcName != dstName {
		if err := op.Rename(ctx, storage, srcActual, dstName); err != nil {
			if unsupported(err) {
				return cant
			}
			return err
		}
	}
	return nil
}
