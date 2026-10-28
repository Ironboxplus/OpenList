package rclonefs

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/hash"
)

// NewPutStream turns what rclone hands a backend's Put into the stream
// OpenList's drivers expect.
//
// rclone gives a sequential reader plus the source object. When the source
// can be range-read (a local file, another OpenList mount, any rclone
// backend), the stream is built over the SOURCE, not over `in`:
//
//   - it is a *stream.SeekableStream with no Reader and a range-reading Link.
//     Drivers test for that concrete type (115_open: driver.go Put) and only
//     then hash and upload by range instead of first caching the whole file
//     to disk — the difference between reading a 50 GB file once and writing
//     a 50 GB temp copy of it.
//   - the source's SHA1 is pre-filled, so a rapid-upload driver goes straight
//     to its hash pre-check. On a hit nothing is transferred at all.
//
// `in` is then left unread. Only a source that can't be opened by range falls
// back to streaming `in`, with the driver caching as it always has.
func NewPutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, name string) (model.FileStreamer, error) {
	file, _, err := newPutStream(ctx, in, src, name)
	return file, err
}

// newPutStream also returns a counter of the bytes read from the source, which
// is how Put tells a rapid upload (the drive already had the content; only the
// hash prefix was read) from a real one.
func newPutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, name string) (model.FileStreamer, *atomic.Int64, error) {
	read := new(atomic.Int64)
	obj := &model.Object{
		Name:     name,
		Size:     src.Size(),
		Modified: src.ModTime(ctx),
		HashInfo: knownHashes(ctx, src),
	}
	srcObj := fs.UnWrapObjectInfo(src)
	if srcObj == nil || src.Size() < 0 {
		return &stream.FileStream{Ctx: ctx, Obj: obj, Reader: &countingReader{r: in, n: read}}, read, nil
	}
	// rclone measures a transfer by what is read from `in`, which stays
	// untouched here; count the range reads against it instead so the job's
	// bytes, speed and bandwidth limit apply to what is actually read.
	acc, _ := in.(*accounting.Account)
	link := &model.Link{
		ContentLength: src.Size(),
		RangeReader: stream.RangeReaderFunc(func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
			opt := &fs.RangeOption{Start: r.Start, End: -1}
			if r.Length >= 0 {
				opt.End = r.Start + r.Length - 1
			}
			rc, err := srcObj.Open(ctx, opt)
			if err != nil {
				return nil, err
			}
			return &sourceReader{ReadCloser: rc, acc: acc, n: read}, nil
		}),
	}
	ss, err := stream.NewSeekableStream(&stream.FileStream{Ctx: ctx, Obj: obj}, link)
	return ss, read, err
}

type sourceReader struct {
	io.ReadCloser
	acc *accounting.Account
	n   *atomic.Int64
}

func (a *sourceReader) Read(p []byte) (int, error) {
	n, err := a.ReadCloser.Read(p)
	if n > 0 {
		a.n.Add(int64(n))
		if a.acc != nil {
			if accErr := a.acc.AccountRead(n); accErr != nil && err == nil {
				err = accErr
			}
		}
	}
	return n, err
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// Upload describes one finished Put, for whoever runs the job.
type Upload struct {
	Remote string
	Size   int64
	// SourceRead is how many bytes were read from the source. Well under
	// Size means the drive took the file by hash (rapid upload) and nothing
	// was transferred; small files read their whole content for the hash
	// either way, so they never count as rapid.
	SourceRead int64
	Err        error
}

func (u Upload) Rapid() bool { return u.Err == nil && u.Size > 0 && u.SourceRead < u.Size }

// Option configures an Fs at NewFs.
type Option func(*Fs)

// WithUploadObserver has every Put on this Fs, and only on this Fs, report to fn.
// It is state of the Fs, not of the context: a driver below the Fs that builds an
// rclonefs Fs of its own (a Cfs storage over another storage) must not report the
// same file again, just as rclone counts bytes in the operations layer and never
// in a backend.
func WithUploadObserver(fn func(Upload)) Option {
	return func(f *Fs) { f.observer = fn }
}

// knownHashes collects the source hashes OpenList drivers use for rapid
// upload. For a local source rclone computes and caches them on the object;
// rclone's post-transfer verification needs the same hash anyway, so asking
// here costs no extra read.
func knownHashes(ctx context.Context, src fs.ObjectInfo) utils.HashInfo {
	hashes := map[*utils.HashType]string{}
	supported := src.Fs().Hashes()
	for _, ty := range []hash.Type{hash.SHA1, hash.MD5} {
		if !supported.Contains(ty) {
			continue
		}
		if sum, err := src.Hash(ctx, ty); err == nil && sum != "" {
			hashes[toOpenListHash(ty)] = sum
		}
	}
	return utils.NewHashInfoByMap(hashes)
}
