package syncjob

import (
	"archive/zip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
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

// packUp is the packing stage of an upload. It walks src and brings the packs
// and the manifest under "<dst>/.oppack/" up to date with every file below
// the threshold. It returns views of src and dst for the main rclone pass: src
// without any small file (they live in packs now), dst without the .oppack
// directories, so the pass moves only the large files and neither re-uploads
// the small ones nor (sync) deletes the packs as extras.
//
// Packs are uploaded as soon as they are full; once all are up, the previous
// manifest is saved as manifest.prev and the new manifest uploaded. That last
// upload is the commit: a run that dies earlier leaves the old manifest valid
// and some orphan packs that the next run numbers past and ignores.
func packUp(ctx context.Context, spec Spec, src, dst fs.Fs) (fs.Fs, fs.Fs, error) {
	stage, err := newStaging(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("pack: staging: %w", err)
	}
	defer stage.close()
	p := &packer{
		opts:      spec.Pack,
		src:       src,
		dst:       dst,
		stage:     stage,
		sync:      spec.Mode == ModeSync,
		dry:       spec.DryRun,
		hidden:    map[string]struct{}{},
		hiddenDst: map[string]struct{}{},
		present:   map[string]struct{}{},
		visited:   map[string]struct{}{},
	}
	if err := p.run(ctx); err != nil {
		return nil, nil, err
	}
	hidden, hiddenDst := p.hidden, p.hiddenDst
	srcView := newViewFs(ctx, src, func(remote string, isDir bool) bool {
		if isDir {
			return isPackDir(remote)
		}
		_, ok := hidden[remote]
		return ok
	})
	dstView := newViewFs(ctx, dst, func(remote string, isDir bool) bool {
		if isDir {
			return isPackDir(remote)
		}
		_, ok := hiddenDst[remote]
		return ok
	})
	return srcView, dstView, nil
}

type packer struct {
	opts  *PackOptions
	src   fs.Fs
	dst   fs.Fs
	stage *stagingArea
	sync  bool
	dry   bool

	m      *manifest
	mObj   fs.Object // the manifest object at dst, nil when there is none
	oldRaw []byte    // its encoded bytes, saved as manifest.prev on commit

	smalls  []fs.Object         // small files at src, in walk order
	hidden  map[string]struct{} // their remotes: kept from the main pass
	present map[string]struct{} // src files that are not small
	loose   []loosePair
	visited map[string]struct{} // src directories walked
	v1      []v1Dir             // version 1 per-directory packs found at dst
	// hiddenDst are loose dst files the main pass must not touch.
	hiddenDst map[string]struct{}
}

// v1Dir is a directory at dst packed the version 1 way (dir/.oppack/index.json).
type v1Dir struct {
	dir string
	m   *manifest
}

// migrant is a v1 member that has no source file left to repack from; it
// moves into the job packs as it is.
type migrant struct {
	d      v1Dir
	name   string
	remote string
	e      fileEntry
}

// loosePair is a small file at src that also exists loose at dst.
type loosePair struct{ src, dst fs.Object }

func (p *packer) run(ctx context.Context) error {
	if err := p.loadManifest(ctx); err != nil {
		return err
	}
	if err := walkDirs(ctx, p.src, func(dir string, files []fs.Object, _ []string) error {
		return p.scan(ctx, dir, files)
	}); err != nil {
		return err
	}
	if err := p.findOrphanV1(ctx); err != nil {
		return err
	}
	addStats(ctx, func(s *PackStats) { s.SmallFiles = int64(len(p.smalls)) })

	m := p.m
	small := make(map[string]fs.Object, len(p.smalls))
	for _, o := range p.smalls {
		small[o.Remote()] = o
	}
	dirty := false

	// Entries whose file is no longer packable: still at src but no longer
	// small (it goes up as an ordinary file), or gone from src. A deletion at
	// src only reaches the manifest in sync mode; copy never deletes.
	var dropped int64
	for name := range m.Files {
		if _, ok := small[name]; ok {
			continue
		}
		if _, there := p.present[name]; there || p.sync {
			m.drop(name)
			dropped++
			dirty = true
		}
	}
	var add []fs.Object
	for _, o := range p.smalls {
		e, ok := m.Files[o.Remote()]
		if !ok {
			add = append(add, o)
			continue
		}
		changed, touch := p.changed(ctx, o, e)
		switch {
		case changed:
			m.drop(o.Remote())
			dropped++
			add = append(add, o)
		case touch:
			e.MTime = formatMTime(o.ModTime(ctx))
			m.Files[o.Remote()] = e
			dirty = true
		}
	}

	// v1 members that exist only in their v1 pack. When src still has the file
	// it is repacked from there (it is missing from the job manifest); when it
	// is gone, sync lets it go and copy keeps it, moved over as it is.
	var migrants []migrant
	for _, d := range p.v1 {
		names := make([]string, 0, len(d.m.Files))
		for name := range d.m.Files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			remote := path.Join(d.dir, name)
			_, packed := m.Files[remote]
			_, isSmall := small[remote]
			_, there := p.present[remote]
			if packed || isSmall || there || p.sync {
				continue
			}
			migrants = append(migrants, migrant{d, name, remote, d.m.Files[name]})
		}
	}

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
	addStats(ctx, func(s *PackStats) { s.DeadRemoved += dropped })

	if p.dry {
		addStats(ctx, func(s *PackStats) { s.V1MigratedDirs += int64(len(p.v1)) })
		var n int64
		for _, o := range add {
			n += o.Size()
		}
		addStats(ctx, func(s *PackStats) {
			s.PackedFiles += int64(len(add))
			s.BytesIn += n
			s.PacksTotal = int64(len(m.Packs))
		})
		return nil
	}
	if len(m.Files) == 0 && len(add) == 0 && len(migrants) == 0 {
		addStats(ctx, func(s *PackStats) { s.PacksTotal = 0 })
		if p.mObj != nil {
			// Nothing left to pack: the packs and manifests go.
			if err := p.removeAll(ctx); err != nil {
				return err
			}
		}
		return p.removeV1(ctx)
	}

	changedAny := len(add) > 0 || len(migrants) > 0 || len(compact) > 0 || len(obsolete) > 0 || dirty || len(p.v1) > 0
	if changedAny {
		b := &packBuilder{p: p, m: m}
		for _, o := range add {
			if err := b.add(ctx, o); err != nil {
				return fmt.Errorf("pack: %w", err)
			}
		}
		if err := b.migrate(ctx, migrants); err != nil {
			return fmt.Errorf("pack: migrate v1 packs: %w", err)
		}
		for _, name := range compact {
			if err := b.compact(ctx, name); err != nil {
				return fmt.Errorf("pack: compact %s: %w", name, err)
			}
			obsolete = append(obsolete, name)
		}
		if err := b.finish(ctx); err != nil {
			return fmt.Errorf("pack: %w", err)
		}
		addStats(ctx, func(s *PackStats) { s.CompactedPacks += int64(len(compact)) })
		for _, name := range obsolete {
			delete(m.Packs, name)
		}
		m.Threshold = p.opts.Threshold
		m.recount()
		if err := p.commit(ctx); err != nil {
			return fmt.Errorf("pack: commit manifest: %w", err)
		}
		// Only now that no manifest refers to them.
		for _, name := range obsolete {
			if err := p.removeObject(ctx, packPath("", name)); err != nil {
				log.Warnf("[sync] pack: can't remove obsolete %s: %v", packPath("", name), err)
			}
		}
	}
	addStats(ctx, func(s *PackStats) { s.PacksTotal = int64(len(m.Packs)) })
	if err := p.removeV1(ctx); err != nil {
		return err
	}
	return p.retireLoose(ctx)
}

// loadManifest reads the job manifest at dst and makes sure new packs are
// numbered past every pack already there.
func (p *packer) loadManifest(ctx context.Context) error {
	m, o, err := loadV2(ctx, p.dst, p.opts.Threshold)
	if err != nil {
		return fmt.Errorf("pack: read manifest: %w", err)
	}
	p.m, p.mObj = m, o
	if o != nil {
		rc, err := o.Open(ctx)
		if err != nil {
			return fmt.Errorf("pack: read manifest: %w", err)
		}
		p.oldRaw, err = io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("pack: read manifest: %w", err)
		}
	}
	entries, err := p.dst.List(ctx, PackDirName)
	if err != nil && !errors.Is(err, fs.ErrorDirNotFound) {
		return fmt.Errorf("pack: list %s: %w", PackDirName, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, path.Base(e.Remote()))
	}
	m.raiseNext(names...)
	return nil
}

// scan sorts one src directory's files into small and not, and notes the
// small ones that also sit loose at dst.
func (p *packer) scan(ctx context.Context, dir string, files []fs.Object) error {
	p.visited[dir] = struct{}{}
	loose, hasPack, err := p.dstFiles(ctx, dir)
	if err != nil {
		return err
	}
	if hasPack {
		if err := p.noteV1(ctx, dir); err != nil {
			return err
		}
	}
	for _, o := range files {
		if o.Size() < 0 || o.Size() >= p.opts.Threshold || !validRel(o.Remote()) {
			p.present[o.Remote()] = struct{}{}
			continue
		}
		p.smalls = append(p.smalls, o)
		p.hidden[o.Remote()] = struct{}{}
		if d, ok := loose[path.Base(o.Remote())]; ok {
			p.loose = append(p.loose, loosePair{o, d})
		}
	}
	return nil
}

// dstFiles lists the loose files of dir at dst. The main pass lists the same
// directories, and OpenList caches listings, so this is not a second round of
// driver calls.
func (p *packer) dstFiles(ctx context.Context, dir string) (files map[string]fs.Object, hasPackDir bool, err error) {
	entries, err := p.dst.List(ctx, dir)
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("list %q at dst: %w", dir, err)
	}
	files = map[string]fs.Object{}
	for _, e := range entries {
		switch x := e.(type) {
		case fs.Directory:
			if path.Base(x.Remote()) == PackDirName {
				hasPackDir = true
			}
		case fs.Object:
			files[path.Base(x.Remote())] = x
		}
	}
	return files, hasPackDir, nil
}

// noteV1 records dir's version 1 pack, if it has one.
func (p *packer) noteV1(ctx context.Context, dir string) error {
	m, _, err := loadV1(ctx, p.dst, dir)
	if err != nil {
		return fmt.Errorf("pack %q: read v1 index: %w", dir, err)
	}
	if m != nil {
		p.v1 = append(p.v1, v1Dir{dir, m})
	}
	return nil
}

// findOrphanV1 finds version 1 packs in directories src no longer has.
func (p *packer) findOrphanV1(ctx context.Context) error {
	return walkDirs(ctx, p.dst, func(dir string, _ []fs.Object, subdirs []string) error {
		if _, ok := p.visited[dir]; ok {
			return nil
		}
		for _, d := range subdirs {
			if d == PackDirName {
				return p.noteV1(ctx, dir)
			}
		}
		return nil
	})
}

// removeV1 deletes the version 1 packs that now live in the job packs. The
// root's v1 pack shares its directory with the job packs, so only its own
// files go.
func (p *packer) removeV1(ctx context.Context) error {
	for _, d := range p.v1 {
		if d.dir == "" {
			if err := p.removeObject(ctx, packPath("", PackIndexName)); err != nil {
				return fmt.Errorf("pack: remove v1 index: %w", err)
			}
			for name := range d.m.Packs {
				if err := p.removeObject(ctx, packPath("", name)); err != nil {
					return fmt.Errorf("pack: remove v1 pack %s: %w", name, err)
				}
			}
		} else if err := p.removePackDir(ctx, d.dir); err != nil {
			return err
		}
		addStats(ctx, func(s *PackStats) { s.V1MigratedDirs++ })
	}
	return nil
}

// removePackDir deletes dir's whole .oppack at dst, index first.
func (p *packer) removePackDir(ctx context.Context, dir string) error {
	pd := path.Join(dir, PackDirName)
	entries, err := p.dst.List(ctx, pd)
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return nil
		}
		return fmt.Errorf("pack %q: list %s: %w", dir, PackDirName, err)
	}
	var objs []fs.Object
	for _, e := range entries {
		if o, ok := e.(fs.Object); ok {
			objs = append(objs, o)
		}
	}
	sort.SliceStable(objs, func(i, j int) bool {
		return path.Base(objs[i].Remote()) == PackIndexName && path.Base(objs[j].Remote()) != PackIndexName
	})
	for _, o := range objs {
		if err := o.Remove(ctx); err != nil {
			return fmt.Errorf("pack %q: remove %s: %w", dir, o.Remote(), err)
		}
	}
	// Local file systems (Windows) can briefly still list files just removed.
	var rerr error
	for i := 0; i < 5; i++ {
		if rerr = p.dst.Rmdir(ctx, pd); rerr == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Warnf("[sync] pack: can't remove empty %s: %v", pd, rerr)
	return nil
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

// sameLoose says whether the loose dst file is the very file that is packed:
// same size and, where dst keeps modification times, the same time to within
// a second; where it doesn't (115), the same SHA1 — without one to compare,
// it is not provably the same.
func (p *packer) sameLoose(ctx context.Context, srcObj, dstObj fs.Object) bool {
	if srcObj.Size() != dstObj.Size() {
		return false
	}
	if p.dst.Precision() != fs.ModTimeNotSupported {
		d := srcObj.ModTime(ctx).Sub(dstObj.ModTime(ctx))
		if d < 0 {
			d = -d
		}
		return d <= max(time.Second, p.dst.Precision())
	}
	a, errA := srcObj.Hash(ctx, rhash.SHA1)
	b, errB := dstObj.Hash(ctx, rhash.SHA1)
	return errA == nil && errB == nil && a != "" && strings.EqualFold(a, b)
}

// retireLoose removes the loose dst copies of files that now live in a pack.
// A copy that differs from what was packed is somebody's other data: it is
// kept, counted as a conflict, and hidden from the main pass so sync leaves
// it alone too.
func (p *packer) retireLoose(ctx context.Context) error {
	for _, pr := range p.loose {
		if p.sameLoose(ctx, pr.src, pr.dst) {
			if err := pr.dst.Remove(ctx); err != nil {
				return fmt.Errorf("remove superseded %s: %w", pr.dst.Remote(), err)
			}
			addStats(ctx, func(s *PackStats) { s.LooseRemoved++ })
			continue
		}
		log.Warnf("[sync] pack: %s exists loose at dst with different content than the packed copy; keeping both", pr.dst.Remote())
		p.hiddenDst[pr.dst.Remote()] = struct{}{}
		addStats(ctx, func(s *PackStats) { s.Conflicts++ })
	}
	return nil
}

// commit saves the previous manifest as manifest.prev, then uploads the new
// one. Uploading the new manifest is what makes the run count.
func (p *packer) commit(ctx context.Context) error {
	if p.oldRaw != nil {
		rel, full := p.stage.newPath()
		if err := os.WriteFile(full, p.oldRaw, 0o644); err != nil {
			return err
		}
		prev, err := p.dst.NewObject(ctx, packPath("", PackManifestPrevName))
		if err != nil && !notFound(err) {
			return err
		}
		if err := p.upload(ctx, rel, packPath("", PackManifestPrevName), prev); err != nil {
			return err
		}
	}
	b, err := p.m.encode()
	if err != nil {
		return err
	}
	rel, full := p.stage.newPath()
	if err := os.WriteFile(full, b, 0o644); err != nil {
		return err
	}
	return p.upload(ctx, rel, packPath("", PackManifestName), p.mObj)
}

func (p *packer) removeObject(ctx context.Context, remote string) error {
	o, err := p.dst.NewObject(ctx, remote)
	if err != nil {
		if notFound(err) {
			return nil
		}
		return err
	}
	return o.Remove(ctx)
}

// removeAll deletes the job's packs and manifests; v1 content in the same
// directory is not ours to touch here.
func (p *packer) removeAll(ctx context.Context) error {
	names := []string{PackManifestName}
	for name := range p.m.Packs {
		names = append(names, name)
	}
	sort.Strings(names[1:])
	for _, name := range names {
		if err := p.removeObject(ctx, packPath("", name)); err != nil {
			return fmt.Errorf("pack: remove %s: %w", name, err)
		}
	}
	if err := p.removeObject(ctx, packPath("", PackManifestPrevName)); err != nil {
		return fmt.Errorf("pack: remove %s: %w", PackManifestPrevName, err)
	}
	if err := p.dst.Rmdir(ctx, PackDirName); err != nil {
		log.Debugf("[sync] pack: %s not removed: %v", PackDirName, err)
	}
	return nil
}

type packBuilder struct {
	p *packer
	m *manifest

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
	name := o.Remote()
	method := zip.Deflate
	if b.p.opts.stores(path.Base(name)) {
		method = zip.Store
	}
	mod := o.ModTime(ctx)
	w, err := b.zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: mod})
	if err != nil {
		return err
	}
	rc, err := o.Open(ctx)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	h := sha1.New()
	n, err := io.Copy(io.MultiWriter(w, h), rc)
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
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
	if err := b.p.upload(ctx, rel, packPath("", name), nil); err != nil {
		return fmt.Errorf("upload %s: %w", name, err)
	}
	addStats(ctx, func(s *PackStats) { s.PacksWritten++; s.BytesOut += w.n })
	return nil
}

// migrate moves v1 members whose source is gone into the job packs without
// recompressing them: the zip entry is copied raw under its full path.
func (b *packBuilder) migrate(ctx context.Context, ms []migrant) error {
	type key struct{ dir, pack string }
	groups := map[key][]migrant{}
	var keys []key
	for _, mg := range ms {
		k := key{mg.d.dir, mg.e.Pack}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], mg)
	}
	for _, k := range keys {
		if err := b.migrateGroup(ctx, k.dir, k.pack, groups[k]); err != nil {
			return fmt.Errorf("%s: %w", packPath(k.dir, k.pack), err)
		}
	}
	return nil
}

func (b *packBuilder) migrateGroup(ctx context.Context, dir, pack string, ms []migrant) error {
	o, err := b.p.dst.NewObject(ctx, packPath(dir, pack))
	if err != nil {
		return err
	}
	_, full := b.p.stage.newPath()
	defer os.Remove(full)
	sum, err := fetchTo(ctx, o, full)
	if err != nil {
		return err
	}
	if want := ms[0].d.m.Packs[pack].SHA1; want != "" && !strings.EqualFold(want, sum) {
		return fmt.Errorf("pack is corrupt (sha1 %s, index says %s)", sum, want)
	}
	zr, err := zip.OpenReader(full)
	if err != nil {
		return err
	}
	defer zr.Close()
	members := map[string]*zip.File{}
	for _, zf := range zr.File {
		members[zf.Name] = zf
	}
	for _, mg := range ms {
		zf := members[mg.name]
		if zf == nil {
			return fmt.Errorf("%s is not in the pack", mg.name)
		}
		if int64(zf.UncompressedSize64) != mg.e.Size {
			return fmt.Errorf("member %s is %d bytes, index says %d", mg.name, zf.UncompressedSize64, mg.e.Size)
		}
		if err := b.open(); err != nil {
			return err
		}
		hdr := zf.FileHeader
		hdr.Name = mg.remote
		w, err := b.zw.CreateRaw(&hdr)
		if err != nil {
			return err
		}
		raw, err := zf.OpenRaw()
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, raw); err != nil {
			return err
		}
		mg.e.Pack = b.name
		b.m.Files[mg.remote] = mg.e
		b.members++
		addStats(ctx, func(s *PackStats) { s.PackedFiles++ })
		if err := b.rollIfFull(ctx); err != nil {
			return err
		}
	}
	return nil
}

// compact re-packs the live members of an old pack into the current one.
func (b *packBuilder) compact(ctx context.Context, name string) error {
	o, err := b.p.dst.NewObject(ctx, packPath("", name))
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
	return fetchHash(ctx, o, path, sha1.New())
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
