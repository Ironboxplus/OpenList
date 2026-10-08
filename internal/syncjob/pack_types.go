package syncjob

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Small-file packing. Cloud drives like 115 charge every file one API call to
// upload and another to download, so a tree of 100k small files spends hours
// in rate limiting. With packing enabled no file below the threshold is
// uploaded on its own: they are zipped, in walk order, into a few packs under
// "<dst>/.oppack/", described by one manifest for the whole job. Everything at
// or above the threshold is transferred as usual. Unpacking reverses it.
//
// Layout at the job's destination root:
//
//	.oppack/manifest.json.gz       the manifest (v2)
//	.oppack/manifest.prev.json.gz  the manifest before the last commit
//	.oppack/p-000001.zip ...       packs; members are named by their full
//	                               path relative to the root, so "unzip p-*.zip"
//	                               there restores the tree without our tools
//
// Version 1 packed each directory separately ("<dir>/.oppack/index.json"); it
// is still read, and migrated away on the next pack run.

const (
	// PackDirName is the directory packs live in.
	PackDirName = ".oppack"
	// PackManifestName is the v2 manifest inside the root PackDirName.
	PackManifestName = "manifest.json.gz"
	// PackManifestPrevName is the manifest as it was before the last commit.
	PackManifestPrevName = "manifest.prev.json.gz"
	// PackIndexName is the v1 per-directory manifest.
	PackIndexName = "index.json"

	DefaultPackThreshold    int64 = 8 << 20
	DefaultPackMaxPackBytes int64 = 1 << 30
)

// DefaultStoreSuffixes are the extensions whose content is already compressed
// or incompressible; their pack members are stored, everything else deflated.
var DefaultStoreSuffixes = []string{
	".pt", ".pth", ".ckpt", ".gz", ".tgz", ".zip", ".npz", ".npy", ".arrow",
	".safetensors", ".bin", ".so", ".cubin", ".png", ".jpg", ".jpeg", ".webp",
	".mp4", ".mkv", ".zst", ".xz", ".bz2", ".7z", ".parquet", ".h5", ".pkl",
	".pickle", ".whl",
}

// PackOptions turns on small-file packing for an upload (copy / sync).
type PackOptions struct {
	Enabled bool `json:"enabled"`
	// Threshold: files smaller than this are small. Default 8 MiB.
	Threshold int64 `json:"threshold"`
	// MaxPackBytes caps one pack; more content goes into further packs.
	// Default 1 GiB. Staging on disk stays within twice this.
	MaxPackBytes int64 `json:"max_pack_bytes"`
	// StoreSuffixes are stored uncompressed. Default DefaultStoreSuffixes.
	StoreSuffixes []string `json:"store_suffixes"`
}

// DefaultPackOptions is the single source of packing defaults; the CLI and
// the web UI display these.
func DefaultPackOptions() PackOptions {
	return PackOptions{
		Enabled:       true,
		Threshold:     DefaultPackThreshold,
		MaxPackBytes:  DefaultPackMaxPackBytes,
		StoreSuffixes: append([]string(nil), DefaultStoreSuffixes...),
	}
}

// normalize fills unset fields with defaults and rejects nonsense.
func (p *PackOptions) normalize() error {
	if p.Threshold < 0 || p.MaxPackBytes < 0 {
		return fmt.Errorf("pack: threshold and max_pack_bytes can't be negative")
	}
	if p.Threshold == 0 {
		p.Threshold = DefaultPackThreshold
	}
	if p.MaxPackBytes == 0 {
		p.MaxPackBytes = DefaultPackMaxPackBytes
	}
	suffixes := p.StoreSuffixes
	if suffixes == nil {
		suffixes = DefaultStoreSuffixes
	}
	norm := make([]string, 0, len(suffixes))
	for _, s := range suffixes {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, ".") {
			s = "." + s
		}
		norm = append(norm, s)
	}
	p.StoreSuffixes = norm
	return nil
}

func (p *PackOptions) stores(name string) bool {
	name = strings.ToLower(name)
	for _, s := range p.StoreSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// PackStats is a job's packing / unpacking progress.
type PackStats struct {
	// Packing: small files seen at src, files put into new packs this run,
	// packs written this run and in the manifest now, source bytes read, pack
	// bytes written, manifest entries dropped because the source file is gone
	// (or no longer small), packs rewritten by compaction, names that exist
	// both loose at dst and in a pack with different content (the loose copy
	// is kept), loose dst copies deleted because a pack now holds them, and
	// v1 per-directory pack directories folded into the job manifest.
	SmallFiles     int64 `json:"small_files"`
	PackedFiles    int64 `json:"packed_files"`
	PacksWritten   int64 `json:"packs_written"`
	PacksTotal     int64 `json:"packs_total"`
	BytesIn        int64 `json:"bytes_in"`
	BytesOut       int64 `json:"bytes_out"`
	DeadRemoved    int64 `json:"dead_removed"`
	CompactedPacks int64 `json:"compacted_packs"`
	Conflicts      int64 `json:"conflicts"`
	LooseRemoved   int64 `json:"loose_removed"`
	V1MigratedDirs int64 `json:"v1_migrated_dirs"`
	// Unpacking: files extracted to dst and their bytes, and packs fetched in
	// full / read by range.
	UnpackedFiles int64 `json:"unpacked_files,omitempty"`
	UnpackedBytes int64 `json:"unpacked_bytes,omitempty"`
	PacksFetched  int64 `json:"packs_fetched,omitempty"`
	PacksRanged   int64 `json:"packs_ranged,omitempty"`
}

// manifest is the job manifest (version 2, keys are paths relative to the job
// root) or a v1 per-directory "index.json" (keys are bare file names).
type manifest struct {
	Version   int                  `json:"version"`
	Threshold int64                `json:"threshold"`
	Next      int                  `json:"next,omitempty"`
	Files     map[string]fileEntry `json:"files"`
	Packs     map[string]packEntry `json:"packs"`
}

type fileEntry struct {
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
	SHA1  string `json:"sha1"`
	Pack  string `json:"pack"`
}

type packEntry struct {
	Bytes int64  `json:"bytes"`
	SHA1  string `json:"sha1"`
	Live  int    `json:"live"`
	Dead  int    `json:"dead"`
}

func newManifest(version int, threshold int64) *manifest {
	return &manifest{Version: version, Threshold: threshold, Files: map[string]fileEntry{}, Packs: map[string]packEntry{}}
}

func (e fileEntry) modTime() time.Time {
	t, err := time.Parse(time.RFC3339Nano, e.MTime)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatMTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

var packNameRE = regexp.MustCompile(`^p-(\d+)\.zip$`)

// raiseNext makes sure new packs are numbered above every pack number in use,
// whether the manifest knows about it or not: a lost manifest, or the packs of
// a failed run, must never be overwritten by a new pack of the same name.
func (m *manifest) raiseNext(names ...string) {
	if m.Next < 1 {
		m.Next = 1
	}
	for name := range m.Packs {
		names = append(names, name)
	}
	for _, name := range names {
		if sm := packNameRE.FindStringSubmatch(name); sm != nil {
			if n, err := strconv.Atoi(sm[1]); err == nil && n >= m.Next {
				m.Next = n + 1
			}
		}
	}
}

func (m *manifest) nextPackName() string {
	m.raiseNext()
	name := fmt.Sprintf("p-%06d.zip", m.Next)
	m.Next++
	return name
}

// recount sets every pack's live count from the files that point to it.
func (m *manifest) recount() {
	live := map[string]int{}
	for _, e := range m.Files {
		live[e.Pack]++
	}
	for name, p := range m.Packs {
		p.Live = live[name]
		m.Packs[name] = p
	}
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

// encode returns the gzip-compressed JSON of m.
func (m *manifest) encode() ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeManifest parses a manifest, gzip-compressed or not, and checks every
// name in it is safe to turn into a path.
func decodeManifest(r io.Reader, origin string) (*manifest, error) {
	b, err := io.ReadAll(io.LimitReader(r, 1<<30))
	if err != nil {
		return nil, err
	}
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", origin, err)
		}
		if b, err = io.ReadAll(zr); err != nil {
			return nil, fmt.Errorf("%s: %w", origin, err)
		}
	}
	m := newManifest(0, 0)
	if err := json.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}
	if m.Files == nil {
		m.Files = map[string]fileEntry{}
	}
	if m.Packs == nil {
		m.Packs = map[string]packEntry{}
	}
	for name, e := range m.Files {
		ok := validName(name)
		if m.Version >= 2 {
			ok = validRel(name)
		}
		if !ok || !validName(e.Pack) {
			return nil, fmt.Errorf("%s: unsafe name %q in manifest", origin, name)
		}
	}
	for name := range m.Packs {
		if !validName(name) {
			return nil, fmt.Errorf("%s: unsafe pack name %q in manifest", origin, name)
		}
	}
	return m, nil
}

// validName rejects anything that could make a file name or pack name land
// outside its own directory when extracted.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && !strings.ContainsRune(name, 0)
}

// validRel accepts a clean relative path whose every segment is a valid name.
func validRel(p string) bool {
	if p == "" || p != path.Clean(p) || strings.ContainsRune(p, 0) || strings.Contains(p, `\`) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if !validName(seg) {
			return false
		}
	}
	return true
}

func packPath(dir, name string) string { return path.Join(dir, PackDirName, name) }
