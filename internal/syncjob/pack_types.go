package syncjob

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// Small-file packing. Cloud drives like 115 charge every file one API call to
// upload and another to download, so a tree of 100k small files spends hours
// in rate limiting. With packing enabled each directory's small files (this
// level only, never recursive) are zipped into a few packs stored under
// "<dir>/.oppack/", described by an index.json manifest; everything else is
// transferred as usual. Unpacking reverses it.

const (
	// PackDirName is the directory a packed directory keeps its packs in.
	PackDirName = ".oppack"
	// PackIndexName is the manifest inside PackDirName.
	PackIndexName = "index.json"

	DefaultPackThreshold    int64 = 8 << 20
	DefaultPackMinFiles           = 8
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
	// MinFiles: a directory with fewer small files at its own level isn't
	// packed. Default 8.
	MinFiles int `json:"min_files"`
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
		MinFiles:      DefaultPackMinFiles,
		MaxPackBytes:  DefaultPackMaxPackBytes,
		StoreSuffixes: append([]string(nil), DefaultStoreSuffixes...),
	}
}

// normalize fills unset fields with defaults and rejects nonsense.
func (p *PackOptions) normalize() error {
	if p.Threshold < 0 || p.MinFiles < 0 || p.MaxPackBytes < 0 {
		return fmt.Errorf("pack: threshold, min_files and max_pack_bytes can't be negative")
	}
	if p.Threshold == 0 {
		p.Threshold = DefaultPackThreshold
	}
	if p.MinFiles == 0 {
		p.MinFiles = DefaultPackMinFiles
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
	// Packing: directories packed, files put into new packs, packs written,
	// source bytes read, pack bytes written, directories with small files but
	// too few to pack, manifest entries dropped because the source file is
	// gone (or no longer small).
	Dirs        int64 `json:"dirs"`
	PackedFiles int64 `json:"packed_files"`
	Packs       int64 `json:"packs"`
	BytesIn     int64 `json:"bytes_in"`
	BytesOut    int64 `json:"bytes_out"`
	SkippedDirs int64 `json:"skipped_dirs"`
	DeadRemoved int64 `json:"dead_removed"`
	// Conflicts: a name that exists both loose and in a pack with different
	// content. Packing keeps the loose dst copy; unpacking takes the pack.
	Conflicts int64 `json:"conflicts"`
	// Unpacking: files extracted to dst and their bytes, and packs fetched in
	// full / read by range.
	UnpackedFiles int64 `json:"unpacked_files,omitempty"`
	UnpackedBytes int64 `json:"unpacked_bytes,omitempty"`
	PacksFetched  int64 `json:"packs_fetched,omitempty"`
	PacksRanged   int64 `json:"packs_ranged,omitempty"`
}

// manifest is "<dir>/.oppack/index.json".
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

func newManifest(threshold int64) *manifest {
	return &manifest{Version: 1, Threshold: threshold, Files: map[string]fileEntry{}, Packs: map[string]packEntry{}}
}

func (e fileEntry) modTime() time.Time {
	t, err := time.Parse(time.RFC3339Nano, e.MTime)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatMTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (m *manifest) nextPackName() string {
	if m.Next <= 0 {
		m.Next = 1
		for name := range m.Packs {
			var n int
			if _, err := fmt.Sscanf(name, "p-%d.zip", &n); err == nil && n >= m.Next {
				m.Next = n + 1
			}
		}
	}
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

// validName rejects anything that could make a pack member or manifest key
// land outside its own directory when extracted.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && !strings.ContainsRune(name, 0)
}

func packPath(dir, name string) string { return path.Join(dir, PackDirName, name) }
