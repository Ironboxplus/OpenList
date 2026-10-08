package check

import (
	"errors"
	"fmt"
	"strings"
)

// Matroska element ids this probe cares about.
const (
	idEBML      = 0x1A45DFA3
	idSegment   = 0x18538067
	idTracks    = 0x1654AE6B
	idCluster   = 0x1F43B675
	idTrackEnt  = 0xAE
	idTrackNum  = 0xD7
	idTrackType = 0x83
	idCodecID   = 0x86
	idLanguage  = 0x22B59C
	idLangBCP47 = 0x22B59D
	idTrackName = 0x536E
)

// TrackSubtitle is the Matroska TrackType of a subtitle track.
const TrackSubtitle = 17

// Track is one Matroska track.
type Track struct {
	Number int
	Type   int // 1 video, 2 audio, 17 subtitle
	Codec  string
	Lang   string
	Name   string
}

// ErrNoTracks means the head of the file ended before the Tracks element.
var ErrNoTracks = errors.New("no Tracks element in the bytes read")

func readID(b []byte, pos int) (id uint32, n int, ok bool) {
	if pos >= len(b) {
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

// readSize returns the payload size; unknown is true for the reserved
// all-ones value (a live-streamed Segment or Cluster).
func readSize(b []byte, pos int) (size int64, n int, unknown, ok bool) {
	if pos >= len(b) {
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

func readUint(b []byte) int {
	v := 0
	for _, x := range b {
		v = v<<8 | int(x)
	}
	return v
}

// ParseMKVTracks reads the Tracks element out of the first bytes of a
// Matroska file. A Tracks element cut off by the end of b yields the tracks
// that were complete.
func ParseMKVTracks(b []byte) ([]Track, error) {
	id, n, ok := readID(b, 0)
	if !ok || id != idEBML {
		return nil, errors.New("not a Matroska file (no EBML header)")
	}
	size, sn, _, ok := readSize(b, n)
	if !ok {
		return nil, errors.New("truncated EBML header")
	}
	pos := n + sn + int(size)
	id, n, ok = readID(b, pos)
	if !ok || id != idSegment {
		return nil, errors.New("no Segment after the EBML header")
	}
	_, sn, _, ok = readSize(b, pos+n)
	if !ok {
		return nil, errors.New("truncated Segment header")
	}
	pos += n + sn // children start here; the Segment's own size never matters
	for {
		id, n, ok = readID(b, pos)
		if !ok {
			return nil, ErrNoTracks
		}
		size, sn, unknown, ok := readSize(b, pos+n)
		if !ok {
			return nil, ErrNoTracks
		}
		start := pos + n + sn
		switch id {
		case idCluster:
			return nil, errors.New("reached the first Cluster without Tracks")
		case idTracks:
			end := len(b)
			if !unknown && int64(start)+size < int64(end) {
				end = start + int(size)
			}
			return parseTrackEntries(b[start:end]), nil
		}
		if unknown || int64(start)+size > int64(len(b)) {
			return nil, ErrNoTracks
		}
		pos = start + int(size)
	}
}

func parseTrackEntries(b []byte) []Track {
	var out []Track
	for pos := 0; pos < len(b); {
		id, n, ok := readID(b, pos)
		if !ok {
			break
		}
		size, sn, _, ok := readSize(b, pos+n)
		if !ok {
			break
		}
		start := pos + n + sn
		end := start + int(size)
		if end > len(b) || end < start {
			break // cut off: this entry is incomplete
		}
		if id == idTrackEnt {
			out = append(out, parseTrackEntry(b[start:end]))
		}
		pos = end
	}
	return out
}

func parseTrackEntry(b []byte) Track {
	t := Track{Lang: "eng"} // Matroska's default when absent
	bcp := ""
	for pos := 0; pos < len(b); {
		id, n, ok := readID(b, pos)
		if !ok {
			break
		}
		size, sn, _, ok := readSize(b, pos+n)
		if !ok {
			break
		}
		start := pos + n + sn
		end := start + int(size)
		if end > len(b) || end < start {
			break
		}
		val := b[start:end]
		switch id {
		case idTrackNum:
			t.Number = readUint(val)
		case idTrackType:
			t.Type = readUint(val)
		case idCodecID:
			t.Codec = strings.TrimRight(string(val), "\x00")
		case idLanguage:
			t.Lang = strings.TrimRight(string(val), "\x00")
		case idLangBCP47:
			bcp = strings.TrimRight(string(val), "\x00")
		case idTrackName:
			t.Name = strings.TrimRight(string(val), "\x00")
		}
		pos = end
	}
	if bcp != "" {
		t.Lang = bcp
	}
	return t
}

// SubtitleTracks keeps the subtitle tracks.
func SubtitleTracks(ts []Track) []Track {
	var out []Track
	for _, t := range ts {
		if t.Type == TrackSubtitle {
			out = append(out, t)
		}
	}
	return out
}

func (t Track) String() string {
	return fmt.Sprintf("%s/%s", t.Codec, t.Lang)
}
