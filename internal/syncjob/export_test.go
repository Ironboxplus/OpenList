package syncjob

import (
	"context"
	"io"

	"github.com/rclone/rclone/fs"
)

var NewFilterOpt = newFilterOpt

// SetRangeThresholds makes small packs take the by-range path in tests.
func SetRangeThresholds(minPack, ratio, chunk int64) (restore func()) {
	a, b, c := rangeMinPack, rangeRatio, rangeChunk
	rangeMinPack, rangeRatio, rangeChunk = minPack, ratio, chunk
	return func() { rangeMinPack, rangeRatio, rangeChunk = a, b, c }
}

// NewRangeReaderAt exposes the ranged pack reader with its request counters.
func NewRangeReaderAt(ctx context.Context, o fs.Object, chunk int64) (io.ReaderAt, func() (requests int, fetched int64)) {
	r := &rangeReaderAt{ctx: ctx, obj: o, size: o.Size(), chunk: chunk}
	return r, func() (int, int64) { return r.requests, r.fetched }
}
