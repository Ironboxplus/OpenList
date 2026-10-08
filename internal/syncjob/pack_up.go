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
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	rhash "github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	log "github.com/sirupsen/logrus"
)

// Test seams: wrapDst decorates dst, separately for the pack line and for the
// main pass; packLineStart runs when the pack line begins, so a test can hold
// it back.
var (
	wrapDst       func(f fs.Fs, packLine bool) fs.Fs
	packLineStart func(context.Context)
)

// packAndTransfer is an upload with packing. It brings the packs and the
// manifest under "<dst>/.oppack/" up to date with every file below the
// threshold, while transfer moves everything else. transfer gets views of src
// without any small file (they live in packs now) and of dst without the
// .oppack directories and without the loose copies of packed files, so it
// neither re-uploads the small ones nor (sync) deletes the packs and loose
// copies as extras.
//
// Packing reads only src and the manifest, so what it asks of dst does not
// depend on the number of directories. The dst view instead notes what the
// transfer's own listings show: loose copies of packed files and version 1
// per-directory packs. Both are dealt with once the manifest is committed.
//
// Packs are uploaded as soon as they are full; once all are up, the previous
// manifest is saved as manifest.prev and the new manifest uploaded. That last
// upload is the commit: a run that dies earlier leaves the old manifest valid
// and some orphan packs that the next run numbers past and ignores.
func packAndTransfer(ctx context.Context, spec Spec, src, dst fs.Fs, transfer func(context.Context, fs.Fs, fs.Fs) error) error {
	mainDst := dst
	if wrapDst != nil {
		dst, mainDst = wrapDst(dst, true), wrapDst(dst, false)
	}
	stage, err := newStaging(ctx)
	if err != nil {
		return fmt.Errorf("pack: staging: %w", err)
	}
	defer stage.close()
	p := &packer{
		opts:    spec.Pack,
		src:     src,
		dst:     dst,
		stage:   stage,
		sync:    spec.Mode == ModeSync,
		dry:     spec.DryRun,
		hidden:  map[string]struct{}{},
		present: map[string]struct{}{},
		v1Dirs:  map[string]struct{}{},
		loose:   map[string]fs.Object{},
	}
	if err := p.plan(ctx); err != nil {
		return err
	}
	srcView := newViewFs(ctx, src, func(remote string, isDir bool) bool {
		if isDir {
			return isPackDir(remote)
		}
		_, ok := p.hidden[remote]
		return ok
	})
	dstView := newViewFs(ctx, mainDst, func(remote string, isDir bool) bool {
		if isDir {
			return isPackDir(remote)
		}
		_, ok := p.packed[remote]
		return ok
	})
	dstView.observe = p.observe

	// The pack line and the transfer run side by side; either failing stops
	// the other.
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var lineErr, xferErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if lineErr = p.line(gctx); lineErr != nil {
			cancel()
		}
	}()
	go func() {
		defer wg.Done()
		if xferErr = transfer(gctx, srcView, dstView); xferErr != nil {
			cancel()
		}
	}()
	wg.Wait()
	// The one that was stopped by the other's failure is just noise.
	if ctx.Err() == nil && lineErr != nil && xferErr != nil {
		switch {
		case errors.Is(xferErr, context.Canceled) && !errors.Is(lineErr, context.Canceled):
			xferErr = nil
		case errors.Is(lineErr, context.Canceled) && !errors.Is(xferErr, context.Canceled):
			lineErr = nil
		}
	}
	err = errors.Join(lineErr, xferErr)
	if lineErr != nil {
		// No commit: the manifest doesn't know the packed files, so what the
		// transfer saw can't be judged.
		return err
	}
	return errors.Join(err, p.tail(ctx))
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

	smalls  []fs.Object // small files at src, in walk order
	small   map[string]fs.Object
	hidden  map[string]struct{} // their remotes: kept from the main pass
	present map[string]struct{} // src files that are not small

	// What the decision step settled, for the pack line to carry out.
	add      []fs.Object
	compact  []string
	obsolete []string
	dirty    bool
	// packed is every file the manifest will hold: dst's copies of them are
	// hidden from the main pass.
	packed map[string]struct{}

	// Filled by the dst view while the main pass lists.
	mu     sync.Mutex
	loose  map[string]fs.Object // loose dst copies of packed files, by remote
	v1Dirs map[string]struct{}  // directories with a version 1 .oppack
	v1     []v1Dir
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

// plan reads the manifest and src and decides what the pack line has to do.
// It is pure computation on top of those reads.
func (p *packer) plan(ctx context.Context) error {
	if err := p.loadManifest(ctx); err != nil {
		return err
	}
	if err := walkDirs(ctx, p.src, func(_ string, files []fs.Object, _ []string) error {
		p.scan(files)
		return nil
	}); err != nil {
		return err
	}
	addStats(ctx, func(s *PackStats) { s.SmallFiles = int64(len(p.smalls)) })

	m := p.m
	p.small = make(map[string]fs.Object, len(p.smalls))
	for _, o := range p.smalls {
		p.small[o.Remote()] = o
	}

	// Entries whose file is no longer packable: still at src but no longer
	// small (it goes up as an ordinary file), or gone from src. A deletion at
	// src only reaches the manifest in sync mode; copy never deletes.
	var dropped int64
	for name := range m.Files {
		if _, ok := p.small[name]; ok {
			continue
		}
		if _, there := p.present[name]; there || p.sync {
			m.drop(name)
			dropped++
			p.dirty = true
		}
	}
	for _, o := range p.smalls {
		e, ok := m.Files[o.Remote()]
		if !ok {
			p.add = append(p.add, o)
			continue
		}
		changed, touch := p.changed(ctx, o, e)
		switch {
		case changed:
			m.drop(o.Remote())
			dropped++
			p.add = append(p.add, o)
		case touch:
			e.MTime = formatMTime(o.ModTime(ctx))
			m.Files[o.Remote()] = e
			p.dirty = true
		}
	}

	m.recount()
	for name, pk := range m.Packs {
		switch {
		case pk.Live == 0 && pk.Dead > 0:
			p.obsolete = append(p.obsolete, name)
		case pk.Dead > pk.Live:
			p.compact = append(p.compact, name)
		}
	}
	sort.Strings(p.compact)
	sort.Strings(p.obsolete)
	addStats(ctx, func(s *PackStats) { s.DeadRemoved += dropped })

	p.packed = make(map[string]struct{}, len(m.Files)+len(p.add))
	for name := range m.Files {
		p.packed[name] = struct{}{}
	}
	for _, o := range p.add {
		p.packed[o.Remote()] = struct{}{}
	}
	return nil
}

// line is the pack line: add, compact, finish, commit, drop the obsolete
// packs. Once it has committed the stage turns to transferring, though the
// main pass has been running all along.
func (p *packer) line(ctx context.Context) error {
	if packLineStart != nil {
		packLineStart(ctx)
	}
	if err := p.build(ctx); err != nil {
		return err
	}
	setStage(ctx, "transferring")
	return nil
}

func (p *packer) build(ctx context.Context) error {
	m := p.m
	if p.dry {
		var n int64
		for _, o := range p.add {
			n += o.Size()
		}
		addStats(ctx, func(s *PackStats) {
			s.PackedFiles += int64(len(p.add))
			s.BytesIn += n
			s.PacksTotal = int64(len(m.Packs))
		})
		return nil
	}
	if len(m.Files) == 0 && len(p.add) == 0 {
		addStats(ctx, func(s *PackStats) { s.PacksTotal = 0 })
		if p.mObj != nil {
			// Nothing left to pack: the packs and manifests go.
			if err := p.removeAll(ctx); err != nil {
				return err
			}
			p.mObj, p.oldRaw = nil, nil
		}
		return nil
	}

	if len(p.add) > 0 || len(p.compact) > 0 || len(p.obsolete) > 0 || p.dirty {
		b := &packBuilder{p: p, m: m}
		for _, o := range p.add {
			if err := b.add(ctx, o); err != nil {
				return fmt.Errorf("pack: %w", err)
			}
		}
		obsolete := p.obsolete
		for _, name := range p.compact {
			if err := b.compact(ctx, name); err != nil {
				return fmt.Errorf("pack: compact %s: %w", name, err)
			}
			obsolete = append(obsolete, name)
		}
		if err := b.finish(ctx); err != nil {
			return fmt.Errorf("pack: %w", err)
		}
		addStats(ctx, func(s *PackStats) { s.CompactedPacks += int64(len(p.compact)) })
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
	return nil
}

// observe is called with every listing the main pass makes of dst. It keeps
// the loose copies of packed files and the version 1 pack directories (the
// root's own is found with the job packs, see loadManifest).
func (p *packer) observe(entries fs.DirEntries) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range entries {
		switch x := e.(type) {
		case fs.Directory:
			if isPackDir(x.Remote()) {
				if dir := path.Dir(x.Remote()); dir != "." {
					p.v1Dirs[dir] = struct{}{}
				}
			}
		case fs.Object:
			if _, ok := p.packed[x.Remote()]; ok {
				p.loose[x.Remote()] = x
			}
		}
	}
}

// tail deals with what the main pass showed of dst. It runs only after the
// manifest is committed, because both the loose copies and the v1 members are
// judged against it.
func (p *packer) tail(ctx context.Context) error {
	if err := p.retireLoose(ctx); err != nil {
		return err
	}
	return p.migrateV1(ctx)
}

// migrateV1 folds the version 1 packs seen at dst into the job packs. Members
// whose src file still exists are in the manifest already (they were added as
// new). Those only the v1 pack has are moved over as they are, in a second
// commit; then the v1 packs go.
func (p *packer) migrateV1(ctx context.Context) error {
	dirs := make([]string, 0, len(p.v1Dirs))
	for dir := range p.v1Dirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		m, _, err := loadV1(ctx, p.dst, dir)
		if err != nil {
			return fmt.Errorf("pack %q: read v1 index: %w", dir, err)
		}
		if m != nil {
			p.v1 = append(p.v1, v1Dir{dir, m})
		}
	}
	if p.dry {
		addStats(ctx, func(s *PackStats) { s.V1MigratedDirs += int64(len(p.v1)) })
		return nil
	}
	if len(p.v1) == 0 {
		return nil
	}
	// When src is gone, sync lets the member go and copy keeps it.
	var migrants []migrant
	for _, d := range p.v1 {
		names := make([]string, 0, len(d.m.Files))
		for name := range d.m.Files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			remote := path.Join(d.dir, name)
			_, packed := p.m.Files[remote]
			_, isSmall := p.small[remote]
			_, there := p.present[remote]
			if packed || isSmall || there || p.sync {
				continue
			}
			migrants = append(migrants, migrant{d, name, remote, d.m.Files[name]})
		}
	}
	if len(migrants) > 0 {
		b := &packBuilder{p: p, m: p.m}
		if err := b.migrate(ctx, migrants); err != nil {
			return fmt.Errorf("pack: migrate v1 packs: %w", err)
		}
		if err := b.finish(ctx); err != nil {
			return fmt.Errorf("pack: %w", err)
		}
		p.m.Threshold = p.opts.Threshold
		p.m.recount()
		if err := p.commit(ctx); err != nil {
			return fmt.Errorf("pack: commit manifest: %w", err)
		}
		addStats(ctx, func(s *PackStats) { s.PacksTotal = int64(len(p.m.Packs)) })
	}
	return p.removeV1(ctx)
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
	for _, n := range names {
		if n == PackIndexName {
			// The root's version 1 pack lives beside the job packs.
			p.v1Dirs[""] = struct{}{}
		}
	}
	return nil
}

// scan sorts one src directory's files into small and not.
func (p *packer) scan(files []fs.Object) {
	for _, o := range files {
		if o.Size() < 0 || o.Size() >= p.opts.Threshold || !validRel(o.Remote()) {
			p.present[o.Remote()] = struct{}{}
			continue
		}
		p.smalls = append(p.smalls, o)
		p.hidden[o.Remote()] = struct{}{}
	}
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

// sameLoose says whether the loose dst file is the very file the manifest
// holds: same size and, where dst keeps modification times, the same time to
// within a second; where it doesn't (115), the same SHA1 — without one to
// compare, it is not provably the same.
func (p *packer) sameLoose(ctx context.Context, e fileEntry, o fs.Object) bool {
	if e.Size != o.Size() {
		return false
	}
	if p.dst.Precision() != fs.ModTimeNotSupported {
		d := e.modTime().Sub(o.ModTime(ctx))
		if d < 0 {
			d = -d
		}
		return d <= max(time.Second, p.dst.Precision())
	}
	sum, err := o.Hash(ctx, rhash.SHA1)
	return err == nil && sum != "" && strings.EqualFold(sum, e.SHA1)
}

// retireLoose removes the loose dst copies of files that now live in a pack.
// A copy that differs from what was packed is somebody's other data: it is
// kept and counted as a conflict. The main pass never saw either kind, so
// sync left them alone. A dry run only counts, and can't judge a file it
// didn't pack.
func (p *packer) retireLoose(ctx context.Context) error {
	remotes := make([]string, 0, len(p.loose))
	for r := range p.loose {
		remotes = append(remotes, r)
	}
	sort.Strings(remotes)
	for _, r := range remotes {
		o := p.loose[r]
		e, ok := p.m.Files[r]
		if !ok {
			continue
		}
		if !p.sameLoose(ctx, e, o) {
			log.Warnf("[sync] pack: %s exists loose at dst with different content than the packed copy; keeping both", r)
			addStats(ctx, func(s *PackStats) { s.Conflicts++ })
			continue
		}
		if !p.dry {
			if err := o.Remove(ctx); err != nil {
				return fmt.Errorf("remove superseded %s: %w", r, err)
			}
		}
		addStats(ctx, func(s *PackStats) { s.LooseRemoved++ })
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
		if _, err := p.upload(ctx, rel, packPath("", PackManifestPrevName), prev); err != nil {
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
	o, err := p.upload(ctx, rel, packPath("", PackManifestName), p.mObj)
	if err != nil {
		return err
	}
	// A later commit of this run replaces this one.
	p.mObj, p.oldRaw = o, b
	return nil
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
	if _, err := b.p.upload(ctx, rel, packPath("", name), nil); err != nil {
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
func (p *packer) upload(ctx context.Context, rel, remote string, existing fs.Object) (fs.Object, error) {
	o, err := p.stage.object(ctx, rel)
	if err != nil {
		return nil, err
	}
	dstObj, err := operations.Copy(ctx, p.dst, existing, remote, o)
	if rerr := os.Remove(filepath.Join(p.stage.root, rel)); err == nil && rerr != nil && !os.IsNotExist(rerr) {
		log.Warnf("[sync] pack: can't remove staged %s: %v", rel, rerr)
	}
	return dstObj, err
}
