// Package ebml reads the EBML (RFC 8794) element framing Matroska is built on,
// shared by the probe in internal/media/check and the subtitle extractor.
package ebml

import "math"

// Matroska element ids (RFC 9559).
const (
	IDEBML            = 0x1A45DFA3
	IDSegment         = 0x18538067
	IDSeekHead        = 0x114D9B74
	IDSeek            = 0x4DBB
	IDSeekID          = 0x53AB
	IDSeekPosition    = 0x53AC
	IDInfo            = 0x1549A966
	IDTimestampScale  = 0x2AD7B1
	IDDuration        = 0x4489
	IDTracks          = 0x1654AE6B
	IDCues            = 0x1C53BB6B
	IDCluster         = 0x1F43B675
	IDTrackEntry      = 0xAE
	IDTrackNumber     = 0xD7
	IDTrackType       = 0x83
	IDDefaultDuration = 0x23E383
	IDCodecID         = 0x86
	IDCodecPrivate    = 0x63A2
	IDLanguage        = 0x22B59C
	IDLanguageBCP47   = 0x22B59D
	IDTrackName       = 0x536E
	IDContentEncs     = 0x6D80
	IDContentEnc      = 0x6240
	IDContentEncScope = 0x5032
	IDContentEncType  = 0x5033
	IDContentComp     = 0x5034
	IDContentCompAlgo = 0x4254
	IDContentCompSet  = 0x4255
	IDCuePoint        = 0xBB
	IDCueTime         = 0xB3
	IDCueTrackPos     = 0xB7
	IDCueTrack        = 0xF7
	IDCueClusterPos   = 0xF1
	IDCueRelativePos  = 0xF0
	IDCueDuration     = 0xB2
	IDSimpleBlock     = 0xA3
	IDBlockGroup      = 0xA0
	IDBlock           = 0xA1
	IDBlockDuration   = 0x9B
)

// ReadID reads an element id (its length marker bits are kept) at b[pos].
func ReadID(b []byte, pos int) (id uint32, n int, ok bool) {
	if pos < 0 || pos >= len(b) {
		return 0, 0, false
	}
	first := b[pos]
	n = 1
	for mask := byte(0x80); mask != 0 && first&mask == 0; mask >>= 1 {
		n++
	}
	if n > 4 || pos+n > len(b) {
		return 0, 0, false
	}
	for i := 0; i < n; i++ {
		id = id<<8 | uint32(b[pos+i])
	}
	return id, n, true
}

// ReadSize returns the payload size at b[pos]; unknown is true for the
// reserved all-ones value (a live-streamed Segment or Cluster).
func ReadSize(b []byte, pos int) (size int64, n int, unknown, ok bool) {
	if pos < 0 || pos >= len(b) {
		return 0, 0, false, false
	}
	first := b[pos]
	n = 1
	mask := byte(0x80)
	for mask != 0 && first&mask == 0 {
		mask >>= 1
		n++
	}
	if mask == 0 || pos+n > len(b) {
		return 0, 0, false, false
	}
	v := int64(first & (mask - 1))
	allOnes := first&(mask-1) == mask-1
	for i := 1; i < n; i++ {
		v = v<<8 | int64(b[pos+i])
		if b[pos+i] != 0xFF {
			allOnes = false
		}
	}
	return v, n, allOnes, true
}

// ReadUint reads a big-endian unsigned integer element payload.
func ReadUint(b []byte) int {
	return int(ReadUint64(b))
}

// ReadUint64 is ReadUint without the int narrowing.
func ReadUint64(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// ReadFloat reads a 4 or 8 byte float element payload (0 for any other size).
func ReadFloat(b []byte) float64 {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(uint32(ReadUint64(b))))
	case 8:
		return math.Float64frombits(ReadUint64(b))
	}
	return 0
}

// Element is one element header found inside a buffer: Start is where its
// payload begins, End where it ends (clamped to the buffer when the element is
// cut off, in which case Cut is set).
type Element struct {
	ID      uint32
	Start   int
	End     int
	Size    int64
	Unknown bool
	Cut     bool
}

// Next reads the element header at b[pos].
func Next(b []byte, pos int) (Element, bool) {
	id, n, ok := ReadID(b, pos)
	if !ok {
		return Element{}, false
	}
	size, sn, unknown, ok := ReadSize(b, pos+n)
	if !ok {
		return Element{}, false
	}
	e := Element{ID: id, Start: pos + n + sn, Size: size, Unknown: unknown}
	e.End = e.Start + int(size)
	if unknown || size < 0 || e.End > len(b) || e.End < e.Start {
		e.End = len(b)
		e.Cut = true
	}
	return e, true
}

// Children walks the elements directly inside b, calling fn with each element
// and its payload. It stops at the first cut-off or malformed element, or when
// fn returns false.
func Children(b []byte, fn func(e Element, payload []byte) bool) {
	for pos := 0; pos < len(b); {
		e, ok := Next(b, pos)
		if !ok || e.Cut {
			return
		}
		if !fn(e, b[e.Start:e.End]) {
			return
		}
		pos = e.End
	}
}
