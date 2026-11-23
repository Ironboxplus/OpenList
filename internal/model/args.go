package model

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

type ListArgs struct {
	ReqPath            string
	S3ShowPlaceholder  bool
	Refresh            bool
	WithStorageDetails bool
	SkipHook           bool
}

type LinkArgs struct {
	IP       string
	Header   http.Header
	Type     string
	Redirect bool
}

// LinkRefresher is a callback function type for refreshing download links
// It returns a new Link and the associated object, or an error
type LinkRefresher func(ctx context.Context) (*Link, Obj, error)

type Link struct {
	URL         string        `json:"url"`    // most common way
	Header      http.Header   `json:"header"` // needed header (for url)
	RangeReader RangeReaderIF `json:"-"`      // recommended way if can't use URL

	Expiration *time.Duration // local cache expiration; not transferred by Clone

	//for accelerating request, use multi-thread downloading
	Concurrency   int   `json:"concurrency"`
	PartSize      int   `json:"part_size"`
	ContentLength int64 `json:"content_length"` // 转码视频、缩略图

	// Gate, when set, counts every connection opened for the link's file against
	// the number its storage allows (see ConnGate). It is set by op.Link for a
	// storage that declares such a limit, not by drivers.
	Gate ConnGate `json:"-"`

	// Refresher is a callback to refresh the link when it expires during long downloads
	// This field is not serialized and is optional - if nil, no refresh will be attempted
	Refresher LinkRefresher `json:"-"`

	utils.SyncClosers `json:"-"`
	// 如果SyncClosers中的资源被关闭后Link将不可用，则此值应为 true
	RequireReference bool `json:"-"`
}

// ConnGate is the count of the connections open to one file of a storage that
// limits them (driver.DownloadLimited), shared by everything that reads the file.
type ConnGate interface {
	// AcquireConn takes one connection's slot, waiting while none is free; no
	// connection goes beyond the limit. A playback takes back the slots that can
	// be given up (background reads, the extra workers of other downloads) and
	// waits for the rest. resumable says that the reader can fetch again what a
	// preempted connection did not deliver; extra, that the read already has
	// another connection open (the other workers of a multi-part download): it
	// waits behind the first connections of playbacks.
	AcquireConn(ctx context.Context, resumable, extra bool) (ConnSlot, error)
}

// ConnSlot is a connection's slot; it is given back by Release, which the
// reader calls when the body was read to the end or is closed.
type ConnSlot interface {
	Release()
	// Preempted is true once a playback has asked for the slot: the reader must
	// give the connection up and, being resumable, open another one later.
	Preempted() bool
	// OnPreempt sets what to call, once, when the slot is asked for, so that a
	// read in progress ends (closing the body).
	OnPreempt(interrupt func())
}

// Clone transfers ownership of l without inheriting its cache expiration.
func (l *Link) Clone() *Link {
	return &Link{
		URL:              l.URL,
		Header:           l.Header,
		RangeReader:      l.RangeReader,
		Concurrency:      l.Concurrency,
		PartSize:         l.PartSize,
		Gate:             l.Gate,
		ContentLength:    l.ContentLength,
		SyncClosers:      utils.NewSyncClosers(l),
		RequireReference: l.RequireReference,
	}
}

type OtherArgs struct {
	Obj    Obj
	Method string
	Data   interface{}
}

type FsOtherArgs struct {
	Path   string      `json:"path" form:"path"`
	Method string      `json:"method" form:"method"`
	Data   interface{} `json:"data" form:"data"`
}

type ArchiveArgs struct {
	Password string
	LinkArgs
}

type ArchiveInnerArgs struct {
	ArchiveArgs
	InnerPath string
}

type ArchiveMetaArgs struct {
	ArchiveArgs
	Refresh bool
}

type ArchiveListArgs struct {
	ArchiveInnerArgs
	Refresh bool
}

type ArchiveDecompressArgs struct {
	ArchiveInnerArgs
	CacheFull     bool
	PutIntoNewDir bool
	Overwrite     bool
}

type SharingListArgs struct {
	Refresh  bool
	Pwd      string
	Token    string // share access token (?auth= / X-Share-Token), stands for Pwd
	ClientIP string
}

type SharingArchiveMetaArgs struct {
	ArchiveMetaArgs
	Pwd      string
	Token    string
	ClientIP string
}

type SharingArchiveListArgs struct {
	ArchiveListArgs
	Pwd      string
	Token    string
	ClientIP string
}

type SharingLinkArgs struct {
	Pwd string
	LinkArgs
}

type RangeReaderIF interface {
	RangeRead(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error)
}
