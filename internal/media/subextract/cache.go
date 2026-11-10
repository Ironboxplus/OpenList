package subextract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ExtractorVersion is part of every cache key: a change in what the extractor
// writes must not be served from files an older one wrote.
const ExtractorVersion = "2"

// diskCache keeps finished tracks as files under dir, least recently used
// removed first once they add up to more than max bytes.
type diskCache struct {
	dir string
	max func() int64
	mu  sync.Mutex
}

var cacheExts = []string{FormatSRT, FormatASS, FormatVTT, FormatSUP}

func (c *diskCache) name(key string, order int) string {
	sum := sha256.Sum256([]byte(key + "\x00" + strconv.Itoa(order) + "\x00" + ExtractorVersion))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:]))
}

func (c *diskCache) Get(key string, order int) (data []byte, format string, ok bool) {
	base := c.name(key, order)
	for _, ext := range cacheExts {
		b, err := os.ReadFile(base + "." + ext)
		if err != nil {
			continue
		}
		now := time.Now()
		_ = os.Chtimes(base+"."+ext, now, now)
		return b, ext, true
	}
	return nil, "", false
}

// fileRecord is what is kept about a file besides its tracks, so that a restart
// does not read the file again to learn it: the subtitle tracks it was probed
// to have, and whether a library prefetch finished it. It is a small file in
// the same directory and goes with the same least-recently-used eviction.
type fileRecord struct {
	Tracks []TrackInfo `json:"tracks"`
	// Done: a prefetch dealt with the file; Cached are the tracks it left on
	// disk, which have to still be there for the file to count as done.
	Done   bool  `json:"done,omitempty"`
	Cached []int `json:"cached,omitempty"`
}

const recordExt = "rec"

func (c *diskCache) recordName(key string) string {
	sum := sha256.Sum256([]byte(key + "\x00record\x00" + ExtractorVersion))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])) + "." + recordExt
}

func (c *diskCache) GetRecord(key string) (fileRecord, bool) {
	var r fileRecord
	name := c.recordName(key)
	b, err := os.ReadFile(name)
	if err != nil || json.Unmarshal(b, &r) != nil {
		return fileRecord{}, false
	}
	now := time.Now()
	_ = os.Chtimes(name, now, now)
	return r, true
}

func (c *diskCache) PutRecord(key string, r fileRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	name := c.recordName(key)
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

func (c *diskCache) Put(key string, order int, format string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	path := c.name(key, order) + "." + format
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	c.evictLocked()
	return nil
}

func (c *diskCache) evictLocked() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		files = append(files, file{filepath.Join(c.dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	limit := c.max()
	if limit <= 0 || total <= limit {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= limit {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
