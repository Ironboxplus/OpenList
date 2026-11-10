package subextract

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	log "github.com/sirupsen/logrus"
)

// retryDelays is the backoff after a failed range read (a 403 from a drive
// that dislikes how many connections it has seen, a 5xx, a dropped stream).
var retryDelays = []time.Duration{500 * time.Millisecond, 2 * time.Second, 5 * time.Second}

// linkSource reads a file through its fs.Link, whatever the driver: the same
// range reader every other consumer of a link gets. Reads are serialised, so a
// drive that answers 403 to a second connection on the same file never sees one.
type linkSource struct {
	rr   model.RangeReaderIF
	size int64
	mu   sync.Mutex
}

// NewLinkSource is a Source over a link. The caller closes the link.
func NewLinkSource(link *model.Link, size int64) (Source, error) {
	rr, err := stream.GetRangeReaderFromLink(size, link)
	if err != nil {
		return nil, err
	}
	return &linkSource{rr: rr, size: size}, nil
}

func (s *linkSource) Size() int64 { return s.size }

func (s *linkSource) ReadAt(ctx context.Context, off int64, n int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	for attempt := 0; ; attempt++ {
		var b []byte
		if b, err = s.readOnce(ctx, off, n); err == nil {
			return b, nil
		}
		if ctx.Err() != nil || attempt >= len(retryDelays) {
			return nil, err
		}
		log.Debugf("subextract: range read at %d failed, retrying: %v", off, err)
		select {
		case <-time.After(retryDelays[attempt]):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *linkSource) readOnce(ctx context.Context, off int64, n int) ([]byte, error) {
	rc, err := s.rr.RangeRead(ctx, http_range.Range{Start: off, Length: int64(n)})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, int64(n)))
}
