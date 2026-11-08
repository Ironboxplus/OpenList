package strm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/media"
)

// The images of a work are kept as references in the database (a data URI, a
// file under the data directory, a VFS file, a URL) and, being served as files,
// need a size that equals the bytes that come out. The tree therefore does not
// know them; a folder's images are read when the folder is first listed (a few
// at a time), and the size of what was read is what is listed. The bytes of
// recently read images stay in memory up to a bound, so the download that
// follows the listing does not read them again; an evicted image is read again
// on demand, and its size is remembered. A reference that cannot be read leaves
// the file out of the listing (and is not tried again for a while).

// the field of a work an image comes from
const (
	imageCover    = "cover"
	imageBackdrop = "backdrop"
	imageStill    = "still" // an episode's, named by mediaImageSrc.episode
)

const (
	mediaImageLimit   = 64 << 20 // bytes of image data kept
	mediaImageWarmers = 4        // images read at a time while listing
	mediaImageFailTTL = 10 * time.Minute
	mediaImageMaxInfo = 50000 // sizes remembered before they are forgotten
)

// mediaImageSrc names where an image's reference is stored. stamp is the
// update time of the work: an updated work is another image.
type mediaImageSrc struct {
	itemID  uint
	field   string
	episode string
	stamp   time.Time
}

func (s mediaImageSrc) key() string {
	return fmt.Sprintf("%d|%s|%s|%d", s.itemID, s.field, s.episode, s.stamp.UnixNano())
}

type imageInfo struct {
	size int64
	ok   bool
	at   time.Time
}

type mediaImages struct {
	mu    sync.Mutex
	info  map[string]imageInfo
	data  map[string][]byte
	order []string // keys of data, oldest first
	used  int64
	limit int64
	load  func(ctx context.Context, src mediaImageSrc) ([]byte, error)
}

func newMediaImages() *mediaImages {
	return &mediaImages{info: map[string]imageInfo{}, data: map[string][]byte{}, limit: mediaImageLimit, load: loadItemImage}
}

// loadItemImage reads the reference from the database and the image it points at.
func loadItemImage(ctx context.Context, src mediaImageSrc) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	item, err := db.GetMediaItemByID(src.itemID)
	if err != nil {
		return nil, err
	}
	ref := ""
	switch src.field {
	case imageCover:
		ref = item.Cover
	case imageBackdrop:
		ref = item.Backdrop
	case imageStill:
		eps, err := db.ListMediaEpisodes(src.itemID)
		if err != nil {
			return nil, err
		}
		for _, e := range eps {
			if e.FileName == src.episode {
				ref = e.Still
				break
			}
		}
	}
	if ref == "" {
		return nil, errors.New("no image reference")
	}
	data, _, err := media.LoadImage(ctx, ref)
	return data, err
}

// size is the size of the image, false when it cannot be read.
func (c *mediaImages) size(ctx context.Context, src mediaImageSrc) (int64, bool) {
	key := src.key()
	c.mu.Lock()
	e, ok := c.info[key]
	c.mu.Unlock()
	if ok && (e.ok || time.Since(e.at) < mediaImageFailTTL) {
		return e.size, e.ok
	}
	if _, err := c.fetch(ctx, src); err != nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info[key].size, true
}

// get returns the bytes of the image.
func (c *mediaImages) get(ctx context.Context, src mediaImageSrc) ([]byte, error) {
	c.mu.Lock()
	data, ok := c.data[src.key()]
	c.mu.Unlock()
	if ok {
		return data, nil
	}
	return c.fetch(ctx, src)
}

// warm reads the sizes of images that are not known yet, a few at a time.
func (c *mediaImages) warm(ctx context.Context, srcs []mediaImageSrc) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, mediaImageWarmers)
	for _, s := range srcs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			c.size(ctx, s)
		}()
	}
	wg.Wait()
}

func (c *mediaImages) fetch(ctx context.Context, src mediaImageSrc) ([]byte, error) {
	key := src.key()
	data, err := c.load(ctx, src)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.info) >= mediaImageMaxInfo {
		c.info = map[string]imageInfo{}
	}
	if err != nil {
		c.info[key] = imageInfo{at: time.Now()}
		return nil, err
	}
	c.info[key] = imageInfo{size: int64(len(data)), ok: true, at: time.Now()}
	if n := int64(len(data)); n <= c.limit/4 {
		if _, have := c.data[key]; !have {
			for c.used+n > c.limit && len(c.order) > 0 {
				old := c.order[0]
				c.order = c.order[1:]
				c.used -= int64(len(c.data[old]))
				delete(c.data, old)
			}
			c.data[key] = data
			c.order = append(c.order, key)
			c.used += n
		}
	}
	return data, nil
}
