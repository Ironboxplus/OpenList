package syncjob

import (
	"archive/zip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	rhash "github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	log "github.com/sirupsen/logrus"
)

// packUp is the packing stage of an upload. It walks src, and for every
// directory with enough small files at its own level it brings the packs and
// the manifest under "<dir>/.oppack/" at dst up to date. It returns views of
// src and dst for the main rclone pass: src without the files now living in
// packs, dst without the .oppack directories.
//
// Packs are uploaded as soon as they are full, then the manifest last; the
// manifest upload is the commit. A run that dies in between leaves orphan
// packs the next run overwrites or ignores, never a manifest that points at
// something missing.
func packUp(ctx context.Context, spec Spec, src, dst fs.Fs) (fs.Fs, fs.Fs, error) {
	stage, err := newStaging(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("pack: staging: %w", err)
	}
	defer stage.close()
	p := &packer{
		opts:   spec.Pack,
		src:    src,
		dst:    dst,
		stage:  stage,
		sync:   spec.Mode == ModeSync,
		dry:    spec.DryRun,
		hidden: map[string]struct{}{},
	}
	if err := walkDirs(ctx, src, func(dir string, files []fs.Object, _ []string) error {
		return p.dir(ctx, dir, files)
	}); err != nil {
		return nil, nil, err
	}
	hidden := p.hidden
	srcView := newViewFs(ctx, src, func(remote string, isDir bool) bool {
		if isDir {
			return isPackDir(remote)
		}
		_, ok := hidden[remote]
		return ok
	})
	dstView := newViewFs(ctx, dst, func(remote string, isDir bool) bool { return isDir && isPackDir(remote) })
	return srcView, dstView, nil
}

type packer struct {
	opts   *PackOptions
	src    fs.Fs
	dst    fs.Fs
	stage  *stagingArea
	sync   bool
	dry    bool
	hidden map[string]struct{}
}

func (p *packer) dir(ctx context.Context, dir string, files []fs.Object) error {
	all := map[string]fs.Object{}
	small := map[string]fs.Object{}
	for _, o := range files {
		name := path.Base(o.Remote())
		all[name] = o
		if o.Size() >= 0 && o.Size() < p.opts.Threshold && validName(name) {
			small[name] = o
		}
	}
	if len(small) == 0 {
		return nil
	}
	if len(small) < p.opts.MinFiles {
		addStats(ctx, func(s *PackStats) { s.SkippedDirs++ })
		return nil
	}
	m, mObj, err := loadManifest(ctx, p.dst, dir, p.opts.Threshold)
	if err != nil {
		return fmt.Errorf("pack %q: read manifest: %w", dir, err)
	}
	for _, o := range small {
		p.hidden[o.Remote()] = struct{}{}
	}
	m.Threshold = p.opts.Threshold
	dirty := false

	// Entries whose file is no longer packable: still at src but no longer
	// small (it goes up as an ordinary file), or gone from src. A deletion at
	// src only reaches the manifest in sync mode; copy never deletes.
	var dropped int64
	for name := range m.Files {
		if _, ok := small[name]; ok {
			continue
		}
		if _, present := all[name]; present || p.sync {
			m.drop(name)
			dropped++
			dirty = true
		}
	}

	var add []fs.Object
	for name, o := range small {
		e, ok := m.Files[name]
		if !ok {
			add = append(add, o)
			continue
		}
		changed, touch := p.changed(ctx, o, e)
		switch {
		case changed:
			m.drop(name)
			dropped++
			add = append(add, o)
		case touch:
			e.MTime = formatMTime(o.ModTime(ctx))
			m.Files[name] = e
			dirty = true
		}
	}
	sort.Slice(add, func(i, j int) bool { return add[i].Remote() < add[j].Remote() })

	m.recount()
	var compact, obsolete []string
	for name, pk := range m.Packs {
		switch {
		case pk.Live == 0 && pk.Dead > 0:
			obsolete = append(obsolete, name)
		case pk.Dead > pk.Live:
			compact = append(compact, name)
		}
	}
	sort.Strings(compact)
	sort.Strings(obsolete)

	addStats(ctx, func(s *PackStats) {
		s.Dirs++
		s.DeadRemoved += dropped
	})
	if p.dry {
		var n int64
		for _, o := range add {
			n += o.Size()
		}
		addStats(ctx, func(s *PackStats) { s.PackedFiles += int64(len(add)); s.BytesIn += n })
		return nil
	}
	if len(add) == 0 && len(compact) == 0 && len(obsolete) == 0 && !dirty {
		return nil
	}

	b := &packBuilder{p: p, dir: dir, m: m}
	for _, o := range add {
		if err := b.add(ctx, o); err != nil {
			return fmt.Errorf("pack %q: %w", dir, err)
		}
	}
	for _, name := range compact {
		if err := b.compact(ctx, name); err != nil {
			return fmt.Errorf("pack %q: compact %s: %w", dir, name, err)
		}
		obsolete = append(obsolete, name)
	}
	if err := b.finish(ctx); err != nil {
		return fmt.Errorf("pack %q: %w", dir, err)
	}
	for _, name := range obsolete {
		delete(m.Packs, name)
	}
	m.recount()
	if err := p.commitManifest(ctx, dir, m, mObj); err != nil {
		return fmt.Errorf("pack %q: write manifest: %w", dir, err)
	}
	// Only now that no manifest refers to them.
	for _, name := range obsolete {
		o, err := p.dst.NewObject(ctx, packPath(dir, name))
		if err == nil {
			err = o.Remove(ctx)
		}
		if err != nil {
			log.Warnf("[sync] pack: can't remove obsolete %s: %v", packPath(dir, name), err)
		}
	}
	return nil
}

// drop removes a file's entry; the member stays in its pack as dead weight
// until the pack is compacted.
func (m *manifest) drop(name string) {
	e, ok := m.Files[name]
	if !ok {
		return
	}
	delete(m.Files, name)
	if pk, ok := m.Packs[e.Pack]; ok {
		pk.Dead++
		m.Packs[e.Pack] = pk
	}
}

// changed compares a source file with its manifest entry: size and mtime
// first, hash only when the mtime disagrees. touch means the content is the
// same but the recorded mtime should follow the source.
func (p *packer) changed(ctx context.Context, o fs.Object, e fileEntry) (changed, touch bool) {
	if o.Size() != e.Size {
		return true, false
	}
	timed := p.src.Precision() != fs.ModTimeNotSupported
	if timed {
		d := o.ModTime(ctx).Sub(e.modTime())
		if d < 0 {
			d = -d
		}
		if d <= time.Second {
			return false, false
		}
	}
	if sum, err := o.Hash(ctx, rhash.SHA1); err == nil && sum != "" {
		if strings.EqualFold(sum, e.SHA1) {
			return false, timed
		}
		return true, false
	}
	// No hash to settle it: with mtimes, a different one means changed;
	// without, size is all there is.
	return timed, false
}

type packBuilder struct {
	p   *packer
	dir string
	m   *manifest

	name    string
	rel     string
	file    *os.File
	zw      *zip.Writer
	w       *countHash
	members int
}

type countHash struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (c *countHash) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.h.Write(b[:n])
	c.n += int64(n)
	return n, err
}

func (b *packBuilder) open() error {
	if b.zw != nil {
		return nil
	}
	rel, full := b.p.stage.newPath()
	f, err := os.Create(full)
	if err != nil {
		return err
	}
	b.name, b.rel, b.file = b.m.nextPackName(), rel, f
	b.w = &countHash{w: f, h: sha1.New()}
	b.zw = zip.NewWriter(b.w)
	b.members = 0
	return nil
}

func (b *packBuilder) add(ctx context.Context, o fs.Object) error {
	if err := b.open(); err != nil {
		return err
	}
	name := path.Base(o.Remote())
	method := zip.Deflate
	if b.p.opts.stores(name) {
		method = zip.Store
	}
	mod := o.ModTime(ctx)
	w, err := b.zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: mod})
	if err != nil {
		return err
	}
	rc, err := o.Open(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", o.Remote(), err)
	}
	h := sha1.New()
	n, err := io.Copy(io.MultiWriter(w, h), rc)
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("read %s: %w", o.Remote(), err)
	}
	// A file that changed while being read is recorded as read; the next run
	// sees the size or mtime differ and packs it again.
	b.m.Files[name] = fileEntry{Size: n, MTime: formatMTime(mod), SHA1: hex.EncodeToString(h.Sum(nil)), Pack: b.name}
	b.members++
	addStats(ctx, func(s *PackStats) { s.PackedFiles++; s.BytesIn += n })
	return b.rollIfFull(ctx)
}

func (b *packBuilder) rollIfFull(ctx context.Context) error {
	if err := b.zw.Flush(); err != nil {
		return err
	}
	if b.w.n >= b.p.opts.MaxPackBytes {
		return b.finish(ctx)
	}
	return nil
}

// finish closes the current pack and uploads it.
func (b *packBuilder) finish(ctx context.Context) error {
	if b.zw == nil {
		return nil
	}
	err := b.zw.Close()
	if cerr := b.file.Close(); err == nil {
		err = cerr
	}
	name, rel, w, members := b.name, b.rel, b.w, b.members
	b.zw, b.file, b.w = nil, nil, nil
	if err != nil {
		return err
	}
	b.m.Packs[name] = packEntry{Bytes: w.n, SHA1: hex.EncodeToString(w.h.Sum(nil)), Live: members}
	if err := b.p.upload(ctx, rel, packPath(b.dir, name), nil); err != nil {
		return fmt.Errorf("upload %s: %w", name, err)
	}
	addStats(ctx, func(s *PackStats) { s.Packs++; s.BytesOut += w.n })
	return nil
}

// compact re-packs the live members of an old pack into the current one.
func (b *packBuilder) compact(ctx context.Context, name string) error {
	o, err := b.p.dst.NewObject(ctx, packPath(b.dir, name))
	if err != nil {
		return err
	}
	_, full := b.p.stage.newPath()
	defer os.Remove(full)
	sum, err := fetchTo(ctx, o, full)
	if err != nil {
		return err
	}
	if want := b.m.Packs[name].SHA1; want != "" && !strings.EqualFold(want, sum) {
		return fmt.Errorf("pack %s is corrupt (sha1 %s, manifest says %s)", name, sum, want)
	}
	zr, err := zip.OpenReader(full)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		e, ok := b.m.Files[zf.Name]
		if !ok || e.Pack != name {
			continue
		}
		if int64(zf.UncompressedSize64) != e.Size {
			return fmt.Errorf("member %s is %d bytes, manifest says %d", zf.Name, zf.UncompressedSize64, e.Size)
		}
		if err := b.open(); err != nil {
			return err
		}
		if err := b.zw.Copy(zf); err != nil {
			return err
		}
		e.Pack = b.name
		b.m.Files[zf.Name] = e
		b.members++
		if err := b.rollIfFull(ctx); err != nil {
			return err
		}
	}
	for n, e := range b.m.Files {
		if e.Pack == name {
			return fmt.Errorf("member %s missing from pack", n)
		}
	}
	return nil
}

// fetchTo downloads o into the local file path and returns its sha1.
func fetchTo(ctx context.Context, o fs.Object, path string) (string, error) {
	rc, err := o.Open(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	h := sha1.New()
	_, err = io.Copy(io.MultiWriter(f, h), rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// upload copies a staged file to dst, replacing existing when given, and
// frees the staged copy.
func (p *packer) upload(ctx context.Context, rel, remote string, existing fs.Object) error {
	o, err := p.stage.object(ctx, rel)
	if err != nil {
		return err
	}
	_, err = operations.Copy(ctx, p.dst, existing, remote, o)
	if rerr := os.Remove(filepath.Join(p.stage.root, rel)); err == nil && rerr != nil && !os.IsNotExist(rerr) {
		log.Warnf("[sync] pack: can't remove staged %s: %v", rel, rerr)
	}
	return err
}

func (p *packer) commitManifest(ctx context.Context, dir string, m *manifest, existing fs.Object) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	rel, full := p.stage.newPath()
	if err := os.WriteFile(full, b, 0o644); err != nil {
		return err
	}
	return p.upload(ctx, rel, packPath(dir, PackIndexName), existing)
}
