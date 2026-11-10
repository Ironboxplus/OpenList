package subextract

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ExtractorVersion is part of every cache key: a change in what the extractor
// writes must not be served from files an older one wrote.
const ExtractorVersion = "1"

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
