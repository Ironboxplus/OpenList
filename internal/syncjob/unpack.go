package syncjob

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	rhash "github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	log "github.com/sirupsen/logrus"
)

// legacyIndexNames are manifests of hand-made archives (one row per archive:
// dir, archive, files, bytes, sha256), looked for at the root of src.
var legacyIndexNames = []string{"PACKED.tsv", "PACKED2.tsv"}

// Reading only a few members of a big pack by range beats fetching the whole
// pack once the pack is large and the wanted share small. Variables so tests
// can reach the range path with small files.
var (
	rangeMinPack int64 = 16 << 20
	rangeRatio   int64 = 4
	rangeChunk   int64 = 1 << 20
)

// unpackDown is the unpacking stage of a download: every ".oppack" directory
// at src is extracted to the matching directory at dst (only members dst lacks
// or has different), and so are the legacy archives listed in PACKED.tsv /
// PACKED2.tsv. It returns views for the main rclone pass that leave out what
// was handled here, so the pass neither re-copies the packs and archives nor
// (sync) deletes the extracted files as extras at dst.
//
// Integrity failures (bad pack checksum, bad member, bad archive sha256) don't
// stop other directories; they are returned as deferred and fail the job after
// the main pass.
func unpackDown(ctx context.Context, spec Spec, src, dst fs.Fs) (unpackResult, error) {
	stage, err := newStaging(ctx)
	if err != nil {
		return unpackResult{}, fmt.Errorf("unpack: staging: %w", err)
	}
	defer stage.close()
	u := &unpacker{
		src:       src,
		dst:       dst,
		stage:     stage,
		dry:       spec.DryRun,
		hiddenSrc: map[string]struct{}{},
		hiddenDst: map[string]struct{}{},
		dstDirs:   map[string]map[string]fs.Object{},
		handled:   map[string]struct{}{},
	}

	var packDirs []string
	var legacy []string
	srcFiles := map[string]struct{}{}
	err = walkDirs(ctx, src, func(dir string, files []fs.Object, subdirs []string) error {
		for _, o := range files {
			srcFiles[o.Remote()] = struct{}{}
			if dir == "" {
				for _, l := range legacyIndexNames {
					if path.Base(o.Remote()) == l {
						legacy = append(legacy, l)
						u.hiddenSrc[o.Remote()] = struct{}{}
					}
				}
			}
		}
		for _, s := range subdirs {
			if s == PackDirName {
				packDirs = append(packDirs, dir)
			}
		}
		return nil
	})
	if err != nil {
		return unpackResult{}, err
	}

	// The job manifest (v2) first, so its entries win over any v1 per-directory
	// manifest left from before a migration.
	for _, dir := range packDirs {
		if dir != "" {
			continue
		}
		m, _, err := loadV2(ctx, src, 0)
		if err != nil {
			u.fail("%s: %v", PackDirName, err)
			continue
		}
		if len(m.Files) > 0 || len(m.Packs) > 0 {
			if err := u.unpackManifest(ctx, "", m, true, srcFiles); err != nil {
				return unpackResult{}, err
			}
		}
	}
	for _, dir := range packDirs {
		if err := ctx.Err(); err != nil {
			return unpackResult{}, err
		}
		m, _, err := loadV1(ctx, src, dir)
		if err != nil {
			u.fail("%q: %v", dir, err)
			continue
		}
		if m == nil {
			continue
		}
		if err := u.unpackManifest(ctx, dir, m, false, srcFiles); err != nil {
			return unpackResult{}, err
		}
	}
	for _, name := range legacy {
		if err := u.unpackLegacy(ctx, name); err != nil {
			return unpackResult{}, err
		}
	}

	hiddenSrc, hiddenDst, prefixes := u.hiddenSrc, u.hiddenDst, u.hiddenPrefixes
	srcView := newViewFs(ctx, src, func(remote string, isDir bool) bool {
		if isDir {
			return isPackDir(remote)
		}
		_, ok := hiddenSrc[remote]
		return ok
	})
	dstView := newViewFs(ctx, dst, func(remote string, isDir bool) bool {
		if isDir {
			for _, p := range prefixes {
				if remote == p {
					return true
				}
			}
			return false
		}
		_, ok := hiddenDst[remote]
		return ok
	})
	return unpackResult{src: srcView, dst: dstView, deferred: errors.Join(u.errs...)}, nil
}

type unpackResult struct {
	src, dst fs.Fs
	// deferred holds integrity failures to fail the job with once the main
	// pass has run.
	deferred error
}

type unpacker struct {
	src, dst fs.Fs
	stage    *stagingArea
	dry      bool

	hiddenSrc      map[string]struct{}
	hiddenDst      map[string]struct{}
	hiddenPrefixes []string
	dstDirs        map[string]map[string]fs.Object
	handled        map[string]struct{} // remotes some manifest already covers
	errs           []error
}

func (u *unpacker) fail(format string, args ...any) {
	err := fmt.Errorf(format, args...)
	log.Warnf("[sync] unpack: %v", err)
	u.errs = append(u.errs, err)
}

// dstDir lists a directory at dst once; a missing directory is empty.
func (u *unpacker) dstDir(ctx context.Context, dir string) (map[string]fs.Object, error) {
	if m, ok := u.dstDirs[dir]; ok {
		return m, nil
	}
	m := map[string]fs.Object{}
	entries, err := u.dst.List(ctx, dir)
	if err != nil && !errors.Is(err, fs.ErrorDirNotFound) {
		return nil, fmt.Errorf("list %s at dst: %w", dir, err)
	}
	for _, e := range entries {
		if o, ok := e.(fs.Object); ok {
			m[path.Base(o.Remote())] = o
		}
	}
	u.dstDirs[dir] = m
	return m, nil
}

// upToDate says whether the dst object already is the manifest entry.
func (u *unpacker) upToDate(ctx context.Context, o fs.Object, e fileEntry) bool {
	if o == nil || o.Size() != e.Size {
		return false
	}
	if u.dst.Precision() != fs.ModTimeNotSupported {
		want := e.modTime()
		if want.IsZero() {
			return true
		}
		d := o.ModTime(ctx).Sub(want)
		if d < 0 {
			d = -d
		}
		return d <= time.Second
	}
	if e.SHA1 != "" {
		if sum, err := o.Hash(ctx, rhash.SHA1); err == nil && sum != "" {
			return strings.EqualFold(sum, e.SHA1)
		}
	}
	return true
}

// member is one file to extract: where it goes at dst, its name inside the
// pack, and what the manifest says about it.
type member struct {
	remote  string
	zipName string
	e       fileEntry
}

// unpackManifest extracts what dst lacks of one manifest. For the job manifest
// (v2) names are paths relative to the root and packs sit in the root's
// .oppack; for a v1 manifest names are files of dir and its packs are in
// dir/.oppack.
func (u *unpacker) unpackManifest(ctx context.Context, dir string, m *manifest, v2 bool, srcFiles map[string]struct{}) error {
	needed := map[string][]member{}
	for name, e := range m.Files {
		remote := path.Join(dir, name)
		if v2 {
			remote = name
		}
		if _, ok := u.handled[remote]; ok {
			continue
		}
		u.handled[remote] = struct{}{}
		if _, ok := srcFiles[remote]; ok {
			// Two sources for one name: the pack member wins, and the loose
			// file is kept out of the transfer so it can't overwrite it.
			u.hiddenSrc[remote] = struct{}{}
			addStats(ctx, func(s *PackStats) { s.Conflicts++ })
		}
		u.hiddenDst[remote] = struct{}{}
		existing, err := u.dstDir(ctx, path.Dir(remote))
		if err != nil {
			return err
		}
		if !u.upToDate(ctx, existing[path.Base(remote)], e) {
			needed[e.Pack] = append(needed[e.Pack], member{remote: remote, zipName: name, e: e})
		}
	}
	packs := make([]string, 0, len(needed))
	for p := range needed {
		packs = append(packs, p)
	}
	sort.Strings(packs)
	for _, p := range packs {
		ms := needed[p]
		sort.Slice(ms, func(i, j int) bool { return ms[i].remote < ms[j].remote })
		if u.dry {
			var n int64
			for _, mb := range ms {
				n += mb.e.Size
			}
			addStats(ctx, func(s *PackStats) { s.UnpackedFiles += int64(len(ms)); s.UnpackedBytes += n })
			continue
		}
		if err := u.extractPack(ctx, packPath(dir, p), m.Packs[p].SHA1, ms); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			u.fail("pack %s: %v", packPath(dir, p), err)
		}
	}
	return nil
}

// extractPack opens a pack (fetching it whole, or by range when only a small
// part of a big pack is wanted) and extracts the given members to dst.
func (u *unpacker) extractPack(ctx context.Context, packRemote, packSHA string, ms []member) error {
	po, err := u.src.NewObject(ctx, packRemote)
	if err != nil {
		return err
	}
	var want int64
	for _, mb := range ms {
		want += mb.e.Size
	}
	var zr *zip.Reader
	if po.Size() >= rangeMinPack && want*rangeRatio < po.Size() {
		ra := &rangeReaderAt{ctx: ctx, obj: po, size: po.Size(), chunk: rangeChunk}
		if zr, err = zip.NewReader(ra, po.Size()); err != nil {
			return err
		}
		addStats(ctx, func(s *PackStats) { s.PacksRanged++ })
	} else {
		_, full := u.stage.newPath()
		defer os.Remove(full)
		sum, err := fetchTo(ctx, po, full)
		if err != nil {
			return err
		}
		if packSHA != "" && !strings.EqualFold(packSHA, sum) {
			return fmt.Errorf("pack is corrupt: sha1 %s, manifest says %s", sum, packSHA)
		}
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if zr, err = zip.NewReader(f, st.Size()); err != nil {
			return err
		}
		addStats(ctx, func(s *PackStats) { s.PacksFetched++ })
	}
	zfiles := map[string]*zip.File{}
	for _, zf := range zr.File {
		zfiles[zf.Name] = zf
	}
	for _, mb := range ms {
		if err := ctx.Err(); err != nil {
			return err
		}
		zf := zfiles[mb.zipName]
		if zf == nil {
			u.fail("%s is not in pack %s", mb.zipName, packRemote)
			continue
		}
		existing, err := u.dstDir(ctx, path.Dir(mb.remote))
		if err != nil {
			return err
		}
		if err := u.extractMember(ctx, zf, mb.remote, mb.e, existing[path.Base(mb.remote)]); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			u.fail("%s: %v", mb.remote, err)
		}
	}
	return nil
}

// extractMember writes one member to staging, checks it against the manifest,
// and only then copies it to dst, so a bad member never reaches dst.
func (u *unpacker) extractMember(ctx context.Context, zf *zip.File, remote string, e fileEntry, existing fs.Object) error {
	if int64(zf.UncompressedSize64) != e.Size {
		return fmt.Errorf("member is %d bytes, manifest says %d", zf.UncompressedSize64, e.Size)
	}
	rel, full := u.stage.newPath()
	defer os.Remove(full)
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.Create(full)
	if err != nil {
		return err
	}
	h := sha1.New()
	n, err := io.Copy(io.MultiWriter(f, h), rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != e.Size {
		return fmt.Errorf("extracted %d bytes, manifest says %d", n, e.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); e.SHA1 != "" && !strings.EqualFold(sum, e.SHA1) {
		return fmt.Errorf("sha1 %s, manifest says %s", sum, e.SHA1)
	}
	mt := e.modTime()
	if mt.IsZero() {
		mt = zf.Modified
	}
	if !mt.IsZero() {
		if err := os.Chtimes(full, mt, mt); err != nil {
			return err
		}
	}
	return u.put(ctx, rel, remote, n, existing)
}

// put copies a staged file to dst and checks the size dst reports.
func (u *unpacker) put(ctx context.Context, rel, remote string, size int64, existing fs.Object) error {
	so, err := u.stage.object(ctx, rel)
	if err != nil {
		return err
	}
	no, err := operations.Copy(ctx, u.dst, existing, remote, so)
	if err != nil {
		return err
	}
	if no != nil && no.Size() != size {
		_ = no.Remove(ctx)
		return fmt.Errorf("wrote %d bytes to dst, expected %d", no.Size(), size)
	}
	addStats(ctx, func(s *PackStats) { s.UnpackedFiles++; s.UnpackedBytes += size })
	return nil
}

// rangeReaderAt reads an object by range through a one-chunk cache, which is
// all a zip reader needs: the central directory sits at the end, members are
// read front to back.
type rangeReaderAt struct {
	ctx   context.Context
	obj   fs.Object
	size  int64
	chunk int64

	mu       sync.Mutex
	off      int64
	buf      []byte
	requests int
	fetched  int64
}

func (r *rangeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}
		if r.buf == nil || pos < r.off || pos >= r.off+int64(len(r.buf)) {
			if err := r.load(pos); err != nil {
				return n, err
			}
		}
		n += copy(p[n:], r.buf[pos-r.off:])
	}
	return n, nil
}

// load fetches the chunk containing pos.
func (r *rangeReaderAt) load(pos int64) error {
	start := pos / r.chunk * r.chunk
	end := min(start+r.chunk, r.size)
	rc, err := r.obj.Open(r.ctx, &fs.RangeOption{Start: start, End: end - 1})
	if err != nil {
		return err
	}
	defer rc.Close()
	buf := make([]byte, end-start)
	if _, err := io.ReadFull(rc, buf); err != nil {
		return fmt.Errorf("range %d-%d of %s: %w", start, end-1, r.obj.Remote(), err)
	}
	r.off, r.buf = start, buf
	r.requests++
	r.fetched += int64(len(buf))
	return nil
}

// --- legacy archives -------------------------------------------------------

type legacyRow struct {
	dir, archive, sha256 string
}

func parseLegacy(r io.Reader) ([]legacyRow, error) {
	var rows []legacyRow
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if ln == 1 && strings.EqualFold(strings.TrimSpace(cols[0]), "dir") {
			continue
		}
		if len(cols) < 2 {
			return nil, fmt.Errorf("line %d: want tab-separated dir, archive, files, bytes, sha256", ln)
		}
		row := legacyRow{dir: cleanRel(cols[0]), archive: cleanRel(cols[1])}
		if len(cols) > 4 {
			row.sha256 = strings.ToLower(strings.TrimSpace(cols[4]))
		}
		if row.dir == "" || row.archive == "" {
			return nil, fmt.Errorf("line %d: empty dir or archive", ln)
		}
		rows = append(rows, row)
	}
	return rows, sc.Err()
}

func (u *unpacker) unpackLegacy(ctx context.Context, index string) error {
	o, err := u.src.NewObject(ctx, index)
	if err != nil {
		return err
	}
	rc, err := o.Open(ctx)
	if err != nil {
		return err
	}
	rows, err := parseLegacy(rc)
	_ = rc.Close()
	if err != nil {
		u.fail("%s: %v", index, err)
		return nil
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		ao, err := u.src.NewObject(ctx, row.archive)
		if err != nil {
			u.fail("%s: archive %s: %v", index, row.archive, err)
			continue
		}
		u.hiddenSrc[ao.Remote()] = struct{}{}
		u.hiddenPrefixes = append(u.hiddenPrefixes, row.dir)
		if u.dry {
			continue
		}
		if err := u.extractLegacy(ctx, row, ao); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			u.fail("%s: %s: %v", index, row.archive, err)
		}
	}
	return nil
}

// extractLegacy verifies the archive's sha256 before anything is written,
// then unpacks it into dirname(row.dir) at dst.
func (u *unpacker) extractLegacy(ctx context.Context, row legacyRow, ao fs.Object) error {
	_, full := u.stage.newPath()
	defer os.Remove(full)
	sum, err := fetchHash(ctx, ao, full, sha256.New())
	if err != nil {
		return err
	}
	if row.sha256 == "" {
		log.Warnf("[sync] unpack: %s has no sha256 in its index; extracting unverified", row.archive)
	} else if sum != row.sha256 {
		return fmt.Errorf("sha256 %s does not match the index (%s); refusing to unpack", sum, row.sha256)
	}
	parent := path.Dir(row.dir)
	if parent == "." {
		parent = ""
	}
	switch name := strings.ToLower(row.archive); {
	case strings.HasSuffix(name, ".zip"):
		zr, err := zip.OpenReader(full)
		if err != nil {
			return err
		}
		defer zr.Close()
		for _, zf := range zr.File {
			if err := ctx.Err(); err != nil {
				return err
			}
			if zf.FileInfo().IsDir() {
				if err := u.legacyDir(ctx, parent, zf.Name); err != nil {
					return err
				}
				continue
			}
			if !zf.Mode().IsRegular() {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			err = u.legacyFile(ctx, parent, zf.Name, int64(zf.UncompressedSize64), zf.Modified, rc)
			_ = rc.Close()
			if err != nil {
				return err
			}
		}
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			switch h.Typeflag {
			case tar.TypeDir:
				err = u.legacyDir(ctx, parent, h.Name)
			case tar.TypeReg:
				err = u.legacyFile(ctx, parent, h.Name, h.Size, h.ModTime, tr)
			}
			if err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported archive type (want .tar.gz, .tgz or .zip)")
	}
	return nil
}

// safeEntry turns an archive entry name into a path relative to the target
// directory, refusing anything that would climb out of it.
func safeEntry(name string) (string, error) {
	name = strings.ReplaceAll(name, `\`, "/")
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", fmt.Errorf("unsafe entry name %q", name)
		}
	}
	rel := strings.TrimPrefix(path.Clean("/"+name), "/")
	if rel == "" {
		return "", fmt.Errorf("empty entry name %q", name)
	}
	return rel, nil
}

func (u *unpacker) legacyDir(ctx context.Context, parent, name string) error {
	rel, err := safeEntry(name)
	if err != nil {
		return err
	}
	return u.dst.Mkdir(ctx, path.Join(parent, rel))
}

func (u *unpacker) legacyFile(ctx context.Context, parent, name string, size int64, mtime time.Time, r io.Reader) error {
	rel, err := safeEntry(name)
	if err != nil {
		return err
	}
	remote := path.Join(parent, rel)
	existing, err := u.dstDir(ctx, path.Dir(remote))
	if err != nil {
		return err
	}
	cur := existing[path.Base(remote)]
	if u.upToDate(ctx, cur, fileEntry{Size: size, MTime: formatMTime(mtime)}) {
		return nil
	}
	srel, full := u.stage.newPath()
	defer os.Remove(full)
	f, err := os.Create(full)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s: extracted %d bytes, header says %d", name, n, size)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			return err
		}
	}
	return u.put(ctx, srel, remote, n, cur)
}

// fetchHash downloads o into the local file path, returning the hex digest.
func fetchHash(ctx context.Context, o fs.Object, path string, h hash.Hash) (string, error) {
	rc, err := o.Open(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(io.MultiWriter(f, h), rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
