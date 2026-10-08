package syncjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/filter"
)

// progress lets the packing stages report to the job that runs them.
type progress struct {
	stage func(string)
	stats func(func(*PackStats))
}

type progressKey struct{}

func withProgress(ctx context.Context, p progress) context.Context {
	return context.WithValue(ctx, progressKey{}, p)
}

func setStage(ctx context.Context, s string) {
	if p, ok := ctx.Value(progressKey{}).(progress); ok && p.stage != nil {
		p.stage(s)
	}
}

func addStats(ctx context.Context, fn func(*PackStats)) {
	if p, ok := ctx.Value(progressKey{}).(progress); ok && p.stats != nil {
		p.stats(fn)
	}
}

// viewFs shows an Fs with some entries hidden. The main rclone pass runs on
// views, which is how it is kept away from files that packing has taken over
// (a filter rule can't say "only this directory's own small files") and from
// the .oppack directories (or sync would delete them as extras at dst).
//
// The view deliberately exposes only the plain fs.Fs interface and features
// rebuilt from it: no ListR, no server-side move. Both would bypass List.
type viewFs struct {
	fs.Fs
	features *fs.Features
	hide     func(remote string, isDir bool) bool
}

func newViewFs(ctx context.Context, inner fs.Fs, hide func(remote string, isDir bool) bool) *viewFs {
	v := &viewFs{Fs: inner, hide: hide}
	v.features = (&fs.Features{
		CanHaveEmptyDirectories: inner.Features().CanHaveEmptyDirectories,
	}).Fill(ctx, v)
	return v
}

func (v *viewFs) Features() *fs.Features { return v.features }

func (v *viewFs) hiddenDir(dir string) bool {
	// Hiding a directory hides everything below it.
	for d := dir; d != "" && d != "."; d = path.Dir(d) {
		if v.hide(d, true) {
			return true
		}
	}
	return false
}

func (v *viewFs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	if v.hiddenDir(dir) {
		return nil, fs.ErrorDirNotFound
	}
	entries, err := v.Fs.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, e := range entries {
		_, isDir := e.(fs.Directory)
		if v.hide(e.Remote(), isDir) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (v *viewFs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if v.hide(remote, false) || v.hiddenDir(path.Dir(remote)) {
		return nil, fs.ErrorObjectNotFound
	}
	return v.Fs.NewObject(ctx, remote)
}

func isPackDir(remote string) bool { return path.Base(remote) == PackDirName }

// walkDirs visits every directory of f that the job's filter includes, parents
// first, with its files and its subdirectory names. Directories named
// .oppack are reported to visit as a name but never entered.
func walkDirs(ctx context.Context, f fs.Fs, visit func(dir string, files []fs.Object, subdirs []string) error) error {
	fi := filter.GetConfig(ctx)
	includeDir := fi.IncludeDirectory(ctx, f)
	var rec func(dir string) error
	rec = func(dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := f.List(ctx, dir)
		if err != nil {
			if errors.Is(err, fs.ErrorDirNotFound) && dir == "" {
				return nil
			}
			return fmt.Errorf("list %s: %w", f.String()+"/"+dir, err)
		}
		var files []fs.Object
		var subdirs []string
		for _, e := range entries {
			switch x := e.(type) {
			case fs.Object:
				if fi.IncludeObject(ctx, x) {
					files = append(files, x)
				}
			case fs.Directory:
				subdirs = append(subdirs, path.Base(x.Remote()))
			}
		}
		if err := visit(dir, files, subdirs); err != nil {
			return err
		}
		for _, name := range subdirs {
			if name == PackDirName {
				continue
			}
			sub := path.Join(dir, name)
			ok, err := includeDir(sub)
			if err != nil {
				return err
			}
			if ok {
				if err := rec(sub); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return rec("")
}

// stagingArea is a job-private directory on local disk where packs are built
// and fetched. It is an rclone Fs so staged files can be copied to dst with
// rclone's own machinery (hashing, rapid upload, retries, accounting).
type stagingArea struct {
	root string
	fs   fs.Fs
	seq  int
}

func newStaging(ctx context.Context) (*stagingArea, error) {
	base := conf.Conf.TempDir
	if base == "" {
		base = os.TempDir()
	}
	base = filepath.Join(base, "oppack")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(base, "job-")
	if err != nil {
		return nil, err
	}
	lf, err := local.NewFs(ctx, "staging", root, configmap.Simple{})
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	return &stagingArea{root: root, fs: lf}, nil
}

func (s *stagingArea) close() { _ = os.RemoveAll(s.root) }

// newPath returns a fresh file name inside the staging directory.
func (s *stagingArea) newPath() (string, string) {
	s.seq++
	rel := fmt.Sprintf("f%06d", s.seq)
	return rel, filepath.Join(s.root, rel)
}

func (s *stagingArea) object(ctx context.Context, rel string) (fs.Object, error) {
	return s.fs.NewObject(ctx, rel)
}

func readManifest(ctx context.Context, o fs.Object) (*manifest, error) {
	rc, err := o.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 256<<20))
	if err != nil {
		return nil, err
	}
	m := newManifest(0)
	if err := json.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s: %w", o.Remote(), err)
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported manifest version %d", o.Remote(), m.Version)
	}
	if m.Files == nil {
		m.Files = map[string]fileEntry{}
	}
	if m.Packs == nil {
		m.Packs = map[string]packEntry{}
	}
	for name, e := range m.Files {
		if !validName(name) || !validName(e.Pack) {
			return nil, fmt.Errorf("%s: unsafe name %q in manifest", o.Remote(), name)
		}
	}
	for name := range m.Packs {
		if !validName(name) {
			return nil, fmt.Errorf("%s: unsafe pack name %q in manifest", o.Remote(), name)
		}
	}
	return m, nil
}

// loadManifest reads dir's manifest from f; a missing one is an empty
// manifest, and the returned object is nil.
func loadManifest(ctx context.Context, f fs.Fs, dir string, threshold int64) (*manifest, fs.Object, error) {
	o, err := f.NewObject(ctx, packPath(dir, PackIndexName))
	if err != nil {
		if errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound) {
			return newManifest(threshold), nil, nil
		}
		return nil, nil, err
	}
	m, err := readManifest(ctx, o)
	if err != nil {
		return nil, nil, err
	}
	return m, o, nil
}

func cleanRel(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	p = path.Clean("/" + p)
	return strings.TrimPrefix(p, "/")
}
