// Package subextract pulls the subtitle tracks out of a video container
// (Matroska/WebM and MP4/MOV) with ranged reads, so a player that is on another
// source of the same video (a transcoded stream that dropped them) can still
// show every one of them. It knows containers, not storage: all bytes come
// through a Source, which the server builds from fs.Link.
//
// References: RFC 9559 (Matroska) Cues and lacing; mpv demux/demux_mkv.c
// (cue index with relative position and duration); FFmpeg matroskadec.c
// (matroska_parse_laces, matroska_decode_buffer, matroska_parse_webvtt),
// assenc.c, supenc.c, movtextdec.c.
package subextract

import (
	"context"
	"errors"
)

// Output formats of an extracted track, all of them ones the players already
// load.
const (
	FormatSRT = "srt"
	FormatASS = "ass"
	FormatVTT = "vtt"
	FormatSUP = "sup"
)

var (
	// ErrUnsupportedContainer is a file that is neither Matroska nor MP4.
	ErrUnsupportedContainer = errors.New("subextract: unsupported container")
	// ErrNoCues is a Matroska file without a Cues element covering its
	// subtitle tracks; finding their blocks would take a scan of the whole
	// file, which is not done.
	ErrNoCues = errors.New("subextract: no cue index for the subtitle tracks")
)

// Source is the bytes of one file. ReadAt returns n bytes at off, fewer only
// at the end of the file. Implementations read one range at a time: a drive
// that refuses a second connection to the same file must never see one.
type Source interface {
	Size() int64
	ReadAt(ctx context.Context, off int64, n int) ([]byte, error)
}

// TrackInfo is one subtitle track of a container.
type TrackInfo struct {
	// Order is the track's position among the container's subtitle tracks,
	// counted from 0: what a player calls the in-band index.
	Order    int    `json:"order"`
	Codec    string `json:"codec"`
	Language string `json:"language"`
	Title    string `json:"title"`
	// Format is the file format the track is extracted to; empty when the
	// codec is not one that can be (Supported is then false).
	Format    string `json:"format"`
	Supported bool   `json:"supported"`
}

// Cue is one decoded subtitle event.
type Cue struct {
	Track int
	// Start and End are milliseconds. PTS is the same instant in 90 kHz ticks,
	// which is what a PGS stream is stamped in.
	Start, End int64
	PTS        int64
	// Text is the cue text; for ASS it is everything after the ReadOrder,
	// "Layer,Style,Name,MarginL,MarginR,MarginV,Effect,Text".
	Text  string
	Order int // ASS ReadOrder
	// Data is a PGS block (one or more segments).
	Data []byte
	// ID and Settings are a WebVTT cue's identifier and settings line.
	ID, Settings string
}

// Item is one unit of work: a block or sample to read and decode.
type Item struct {
	Track int   // subtitle order
	Time  int64 // milliseconds, for ordering
	Pos   int64 // where the read starts
	Size  int64 // payload size when known, 0 when it is learnt on reading
	idx   int   // position in the track's item list
	ref   any
}

// container is what a format gives the extractor.
type container interface {
	Tracks() []TrackInfo
	// Header is the file header of the track's output (ASS script header, VTT
	// header), nil when the format has none.
	Header(order int) []byte
	// Items lists every unit of one track, ordered by time.
	Items(order int) ([]Item, error)
	// Fetch reads and decodes a group of items that lie close together. An item
	// that cannot be decoded is returned in failed and the others go on; err is
	// a failure of the Source (or of ctx), which ends the group.
	Fetch(ctx context.Context, w *window, items []Item) (cues []Cue, failed []Item, err error)
}
