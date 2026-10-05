package rclonefs

import (
	"context"
	"io"
	"strings"
	"time"

	olfs "github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
)

// Object is a file under an OpenList mount path.
type Object struct {
	fs      *Fs
	remote  string
	size    int64
	modTime time.Time
	hashes  utils.HashInfo
}

func (o *Object) Fs() fs.Info                                 { return o.fs }
func (o *Object) Remote() string                              { return o.remote }
func (o *Object) String() string                              { return o.remote }
func (o *Object) ModTime(context.Context) time.Time           { return o.modTime }
func (o *Object) Size() int64                                 { return o.size }
func (o *Object) Storable() bool                              { return true }
func (o *Object) SetModTime(context.Context, time.Time) error { return fs.ErrorCantSetModTime }

func toOpenListHash(ty hash.Type) *utils.HashType {
	switch ty {
	case hash.SHA1:
		return utils.SHA1
	case hash.MD5:
		return utils.MD5
	}
	return nil
}

// Hash returns the hash the storage reported in its listing, or "" when it
// reported none — rclone then falls back to comparing sizes.
func (o *Object) Hash(_ context.Context, ty hash.Type) (string, error) {
	ht := toOpenListHash(ty)
	if ht == nil {
		return "", hash.ErrUnsupported
	}
	// Drivers report hex in whatever case their API returns (115 is upper
	// case); rclone compares hex strings literally and emits lower case.
	return strings.ToLower(o.hashes.GetHash(ht)), nil
}

// Open returns a reader that only resolves the link and starts the request
// on its first Read. rclone opens the source before handing it to the
// destination's Put, and a destination that range-reads the source instead
// (see NewPutStream) never reads it — eager opening would start a download
// per file for nothing.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	r := http_range.Range{Start: 0, Length: -1}
	for _, option := range options {
		switch x := option.(type) {
		case *fs.SeekOption:
			r.Start = x.Offset
		case *fs.RangeOption:
			r.Start, r.Length = x.Decode(o.size)
		default:
			if option.Mandatory() {
				fs.Logf(o, "Unsupported mandatory option: %v", option)
			}
		}
	}
	return &lazyReadCloser{open: func() (io.ReadCloser, error) {
		return o.openRange(ctx, r)
	}}, nil
}

func (o *Object) openRange(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
	link, _, err := olfs.Link(ctx, o.fs.fullPath(o.remote), model.LinkArgs{})
	if err != nil {
		return nil, err
	}
	rr, err := stream.GetRangeReaderFromLink(o.size, link)
	if err != nil {
		_ = link.Close()
		return nil, err
	}
	rc, err := rr.RangeRead(ctx, r)
	if err != nil {
		_ = link.Close()
		return nil, err
	}
	return utils.ReadCloser{Reader: rc, Closer: closerFunc(func() error {
		err := rc.Close()
		_ = link.Close()
		return err
	})}, nil
}

func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	newObj, err := o.fs.Put(ctx, in, fs.NewOverrideRemote(src, o.remote), options...)
	if err != nil {
		return err
	}
	*o = *newObj.(*Object)
	return nil
}

func (o *Object) Remove(ctx context.Context) error {
	return olfs.Remove(ctx, o.fs.fullPath(o.remote))
}

type closerFunc func() error

func (c closerFunc) Close() error { return c() }

type lazyReadCloser struct {
	open func() (io.ReadCloser, error)
	rc   io.ReadCloser
	err  error
}

func (l *lazyReadCloser) Read(p []byte) (int, error) {
	if l.rc == nil && l.err == nil {
		l.rc, l.err = l.open()
	}
	if l.err != nil {
		return 0, l.err
	}
	return l.rc.Read(p)
}

func (l *lazyReadCloser) Close() error {
	if l.rc == nil {
		return nil
	}
	return l.rc.Close()
}
