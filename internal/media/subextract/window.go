package subextract

import (
	"context"
	"fmt"
)

// maxRead bounds one ranged read, so that a size field of a damaged file cannot
// ask for the rest of a terabyte.
const maxRead = 32 << 20

// window is the last range read from a Source. Reads that fall inside it cost
// nothing, which is how a group of nearby blocks is one request.
type window struct {
	src     Source
	off     int64
	buf     []byte
	minRead int
}

func newWindow(src Source, minRead int) *window {
	return &window{src: src, minRead: minRead}
}

// get returns the bytes [off, off+n), clamped to the end of the file.
func (w *window) get(ctx context.Context, off int64, n int) ([]byte, error) {
	size := w.src.Size()
	if off < 0 || n < 0 || n > maxRead {
		return nil, fmt.Errorf("subextract: bad read %d+%d", off, n)
	}
	if off >= size {
		return nil, nil
	}
	if int64(n) > size-off {
		n = int(size - off)
	}
	if off >= w.off && off+int64(n) <= w.off+int64(len(w.buf)) {
		i := int(off - w.off)
		return w.buf[i : i+n], nil
	}
	want := n
	if want < w.minRead {
		want = w.minRead
	}
	if int64(want) > size-off {
		want = int(size - off)
	}
	b, err := w.src.ReadAt(ctx, off, want)
	if err != nil {
		return nil, &SourceError{err}
	}
	w.off, w.buf = off, b
	if len(b) < n {
		n = len(b)
	}
	return b[:n], nil
}
