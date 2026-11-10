package subextract

import (
	"bytes"
	"compress/bzip2"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/media/ebml"
)

// headRead is the first read of a Matroska file: the EBML header, the Segment
// header and, in nearly every file, the SeekHead, Info and Tracks.
const headRead = 64 << 10

// subtitleType is the Matroska TrackType of a subtitle track.
const subtitleType = 17

type mkvEncoding struct {
	order    int
	scope    int
	algo     int // 0 zlib, 1 bzlib, 2 lzo1x, 3 header stripping
	settings []byte
	// unsupported is encryption, or a compression this does not undo.
	unsupported bool
}

type mkvTrack struct {
	Number     int
	Type       int
	Codec      string
	Lang, Name string
	Private    []byte
	DefaultDur int64 // ns
	enc        []mkvEncoding
	order      int // among the subtitle tracks
}

type mkvCue struct {
	Time    int64 // ticks
	Cluster int64 // Segment-relative position of the Cluster
	Rel     int64 // from the Cluster's first child
	Dur     int64 // ticks, -1 when absent
}

type mkvRef struct {
	cue   mkvCue
	track *mkvTrack
}

type mkvFile struct {
	src      Source
	segStart int64
	scale    int64 // ns per tick
	subs     []*mkvTrack
	cues     map[int][]mkvCue // by track number
	haveCues bool
	// hdrGuess is the length of a Cluster's ID and size field, learnt from the
	// first one and checked on every block read.
	hdrGuess int
}

// openMKV reads the structure of a Matroska file: the tracks, and with
// withCues the Cues that say where each subtitle block is.
func openMKV(ctx context.Context, src Source, withCues bool) (*mkvFile, error) {
	w := newWindow(src, headRead)
	head, err := w.get(ctx, 0, headRead)
	if err != nil {
		return nil, err
	}
	id, n, ok := ebml.ReadID(head, 0)
	if !ok || id != ebml.IDEBML {
		return nil, ErrUnsupportedContainer
	}
	size, sn, _, ok := ebml.ReadSize(head, n)
	if !ok {
		return nil, errors.New("subextract: truncated EBML header")
	}
	pos := n + sn + int(size)
	id, n, ok = ebml.ReadID(head, pos)
	if !ok || id != ebml.IDSegment {
		return nil, errors.New("subextract: no Segment after the EBML header")
	}
	_, sn, _, ok = ebml.ReadSize(head, pos+n)
	if !ok {
		return nil, errors.New("subextract: truncated Segment header")
	}
	m := &mkvFile{src: src, segStart: int64(pos + n + sn), scale: 1_000_000, cues: map[int][]mkvCue{}}

	// Where the elements we need are, from walking the top level up to the
	// first Cluster and from the SeekHead, whichever comes first.
	found := map[uint32]int64{}
	seekParsed := false
	for abs := m.segStart; ; {
		hdr, err := w.get(ctx, abs, 16)
		if err != nil {
			return nil, err
		}
		e, ok := ebml.Next(hdr, 0)
		if !ok || e.ID == ebml.IDCluster || e.Unknown {
			break
		}
		switch e.ID {
		case ebml.IDSeekHead:
			if body, _, err := m.elementBody(ctx, w, abs); err == nil {
				m.parseSeekHead(body, found)
				seekParsed = true
			}
		case ebml.IDInfo, ebml.IDTracks, ebml.IDCues:
			if _, dup := found[e.ID]; !dup {
				found[e.ID] = abs
			}
		}
		_, haveTracks := found[ebml.IDTracks]
		_, haveInfo := found[ebml.IDInfo]
		_, haveCues := found[ebml.IDCues]
		if haveTracks && haveInfo && (seekParsed || haveCues) {
			break
		}
		abs += int64(e.Start) + e.Size
		if abs >= src.Size() {
			break
		}
	}

	if at, ok := found[ebml.IDInfo]; ok {
		if body, _, err := m.elementBody(ctx, w, at); err == nil {
			ebml.Children(body, func(e ebml.Element, p []byte) bool {
				if e.ID == ebml.IDTimestampScale {
					if v := ebml.ReadUint64(p); v > 0 {
						m.scale = int64(v)
					}
				}
				return true
			})
		}
	}
	at, ok := found[ebml.IDTracks]
	if !ok {
		return nil, errors.New("subextract: no Tracks element")
	}
	tracksBody, _, err := m.elementBody(ctx, w, at)
	if err != nil {
		return nil, err
	}
	m.parseTracks(tracksBody)

	if withCues && len(m.subs) > 0 {
		if at, ok := found[ebml.IDCues]; ok {
			// A SeekHead that points at no Cues is a file without them.
			body, _, err := m.elementBody(ctx, w, at)
			var se *SourceError
			if errors.As(err, &se) {
				return nil, err
			}
			if err == nil {
				m.parseCues(body)
				m.haveCues = true
				if err := m.learnClusterHeader(ctx, w); err != nil {
					return nil, err
				}
			}
		}
	}
	return m, nil
}

// parseSeekHead adds the positions a SeekHead lists to found, which it does not
// overwrite: what the walk saw with its own eyes wins.
func (m *mkvFile) parseSeekHead(body []byte, found map[uint32]int64) {
	ebml.Children(body, func(e ebml.Element, p []byte) bool {
		if e.ID != ebml.IDSeek {
			return true
		}
		var sid, spos uint64
		var haveID, havePos bool
		ebml.Children(p, func(e ebml.Element, v []byte) bool {
			switch e.ID {
			case ebml.IDSeekID:
				sid, haveID = ebml.ReadUint64(v), true
			case ebml.IDSeekPosition:
				spos, havePos = ebml.ReadUint64(v), true
			}
			return true
		})
		if haveID && havePos {
			if _, dup := found[uint32(sid)]; !dup {
				found[uint32(sid)] = m.segStart + int64(spos)
			}
		}
		return true
	})
}

// elementBody reads the element at abs and returns its payload and header
// length.
func (m *mkvFile) elementBody(ctx context.Context, w *window, abs int64) ([]byte, int, error) {
	hdr, err := w.get(ctx, abs, 16)
	if err != nil {
		return nil, 0, err
	}
	e, ok := ebml.Next(hdr, 0)
	if !ok || e.Unknown || e.Size > maxRead {
		return nil, 0, fmt.Errorf("subextract: bad element at %d", abs)
	}
	body, err := w.get(ctx, abs+int64(e.Start), int(e.Size))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(body)) < e.Size {
		return nil, 0, fmt.Errorf("subextract: element at %d is cut off", abs)
	}
	return body, e.Start, nil
}

func (m *mkvFile) parseTracks(b []byte) {
	ebml.Children(b, func(e ebml.Element, p []byte) bool {
		if e.ID != ebml.IDTrackEntry {
			return true
		}
		t := &mkvTrack{Lang: "eng"} // Matroska's default when absent
		bcp, haveLang := "", false
		ebml.Children(p, func(e ebml.Element, v []byte) bool {
			switch e.ID {
			case ebml.IDTrackNumber:
				t.Number = ebml.ReadUint(v)
			case ebml.IDTrackType:
				t.Type = ebml.ReadUint(v)
			case ebml.IDCodecID:
				t.Codec = strings.TrimRight(string(v), "\x00")
			case ebml.IDCodecPrivate:
				t.Private = append([]byte(nil), v...)
			case ebml.IDLanguage:
				t.Lang, haveLang = strings.TrimRight(string(v), "\x00"), true
			case ebml.IDLanguageBCP47:
				bcp = strings.TrimRight(string(v), "\x00")
			case ebml.IDTrackName:
				t.Name = strings.TrimRight(string(v), "\x00")
			case ebml.IDDefaultDuration:
				t.DefaultDur = int64(ebml.ReadUint64(v))
			case ebml.IDContentEncs:
				ebml.Children(v, func(e ebml.Element, ev []byte) bool {
					if e.ID == ebml.IDContentEnc {
						t.enc = append(t.enc, parseEncoding(ev))
					}
					return true
				})
			}
			return true
		})
		// The ISO 639-2 Language is what FFmpeg and mkvmerge list for a track
		// and what an MP4 track's mdhd holds; the BCP 47 tag (which mkvmerge
		// writes next to it, as "en") only stands in when there is none.
		if bcp != "" && (!haveLang || t.Lang == "und") {
			t.Lang = bcp
		}
		sort.SliceStable(t.enc, func(i, j int) bool { return t.enc[i].order > t.enc[j].order })
		if t.Type == subtitleType {
			t.order = len(m.subs)
			m.subs = append(m.subs, t)
		}
		return true
	})
}

// parseEncoding reads one ContentEncoding. The defaults are the spec's:
// scope 1 (the frames), type 0 (compression), algorithm 0 (zlib).
func parseEncoding(b []byte) mkvEncoding {
	enc := mkvEncoding{scope: 1}
	encType := 0
	ebml.Children(b, func(e ebml.Element, v []byte) bool {
		switch e.ID {
		case 0x5031: // ContentEncodingOrder
			enc.order = ebml.ReadUint(v)
		case ebml.IDContentEncScope:
			enc.scope = ebml.ReadUint(v)
		case ebml.IDContentEncType:
			encType = ebml.ReadUint(v)
		case ebml.IDContentComp:
			ebml.Children(v, func(e ebml.Element, cv []byte) bool {
				switch e.ID {
				case ebml.IDContentCompAlgo:
					enc.algo = ebml.ReadUint(cv)
				case ebml.IDContentCompSet:
					enc.settings = append([]byte(nil), cv...)
				}
				return true
			})
		}
		return true
	})
	if encType != 0 || enc.algo == 2 {
		enc.unsupported = true
	}
	return enc
}

func (m *mkvFile) parseCues(b []byte) {
	want := map[int]bool{}
	for _, t := range m.subs {
		want[t.Number] = true
	}
	ebml.Children(b, func(e ebml.Element, p []byte) bool {
		if e.ID != ebml.IDCuePoint {
			return true
		}
		var cueTime int64
		type pos struct {
			track        int
			cluster, rel int64
			dur          int64
			haveRel      bool
		}
		var poss []pos
		ebml.Children(p, func(e ebml.Element, v []byte) bool {
			switch e.ID {
			case ebml.IDCueTime:
				cueTime = int64(ebml.ReadUint64(v))
			case ebml.IDCueTrackPos:
				ps := pos{dur: -1}
				ebml.Children(v, func(e ebml.Element, pv []byte) bool {
					switch e.ID {
					case ebml.IDCueTrack:
						ps.track = ebml.ReadUint(pv)
					case ebml.IDCueClusterPos:
						ps.cluster = int64(ebml.ReadUint64(pv))
					case ebml.IDCueRelativePos:
						ps.rel, ps.haveRel = int64(ebml.ReadUint64(pv)), true
					case ebml.IDCueDuration:
						ps.dur = int64(ebml.ReadUint64(pv))
					}
					return true
				})
				poss = append(poss, ps)
			}
			return true
		})
		for _, ps := range poss {
			if want[ps.track] && ps.haveRel {
				m.cues[ps.track] = append(m.cues[ps.track], mkvCue{Time: cueTime, Cluster: ps.cluster, Rel: ps.rel, Dur: ps.dur})
			}
		}
		return true
	})
}

// learnClusterHeader reads the header of one Cluster that holds a subtitle
// block: its ID and size field is as long in every Cluster of a file in all
// practice, and a block read that finds it is not checks it again.
func (m *mkvFile) learnClusterHeader(ctx context.Context, w *window) error {
	for _, t := range m.subs {
		if cs := m.cues[t.Number]; len(cs) > 0 {
			n, err := m.clusterHeaderLen(ctx, w, cs[0].Cluster)
			if err != nil {
				return err
			}
			m.hdrGuess = n
			return nil
		}
	}
	return nil
}

func (m *mkvFile) clusterHeaderLen(ctx context.Context, w *window, cluster int64) (int, error) {
	b, err := w.get(ctx, m.segStart+cluster, 16)
	if err != nil {
		return 0, err
	}
	e, ok := ebml.Next(b, 0)
	if !ok || e.ID != ebml.IDCluster {
		return 0, fmt.Errorf("subextract: no Cluster at %d", m.segStart+cluster)
	}
	return e.Start, nil
}

func (m *mkvFile) Tracks() []TrackInfo {
	out := make([]TrackInfo, len(m.subs))
	for i, t := range m.subs {
		f := mkvFormat(t)
		out[i] = TrackInfo{Order: i, Codec: t.Codec, Language: t.Lang, Title: t.Name, Format: f, Supported: f != ""}
	}
	return out
}

// mkvFormat is the output format of a track, "" when it cannot be extracted.
func mkvFormat(t *mkvTrack) string {
	for _, e := range t.enc {
		if e.unsupported && e.scope&1 != 0 {
			return ""
		}
	}
	switch t.Codec {
	case "S_TEXT/UTF8", "S_TEXT/ASCII":
		return FormatSRT
	case "S_TEXT/ASS", "S_TEXT/SSA", "S_ASS", "S_SSA":
		return FormatASS
	case "S_TEXT/WEBVTT":
		return FormatVTT
	case "S_HDMV/PGS":
		return FormatSUP
	}
	return ""
}

func (m *mkvFile) Header(order int) []byte {
	t := m.subs[order]
	switch mkvFormat(t) {
	case FormatASS, FormatVTT:
		return t.Private
	}
	return nil
}

func (m *mkvFile) ticksToMs(t int64) int64 { return t * m.scale / 1_000_000 }

func (m *mkvFile) Items(order int) ([]Item, error) {
	t := m.subs[order]
	if !m.haveCues {
		return nil, ErrNoCues
	}
	cues := m.cues[t.Number]
	if len(cues) == 0 {
		return nil, ErrNoCues
	}
	cs := append([]mkvCue(nil), cues...)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Time < cs[j].Time })
	items := make([]Item, 0, len(cs))
	seen := map[[2]int64]bool{}
	for _, c := range cs {
		k := [2]int64{c.Cluster, c.Rel}
		if seen[k] {
			continue
		}
		seen[k] = true
		items = append(items, Item{
			Track: order,
			Time:  m.ticksToMs(c.Time),
			Pos:   m.segStart + c.Cluster + int64(m.hdrGuess) + c.Rel,
			ref:   mkvRef{cue: c, track: t},
		})
	}
	return items, nil
}

// blockHeadroom is how far past the start of the last block of a group the
// read goes before the block's own size is known.
const blockHeadroom = 4096

func (m *mkvFile) Fetch(ctx context.Context, w *window, items []Item) ([]Cue, error) {
	if len(items) == 0 {
		return nil, nil
	}
	first, last := items[0].Pos, items[0].Pos
	for _, it := range items {
		if it.Pos < first {
			first = it.Pos
		}
		if it.Pos > last {
			last = it.Pos
		}
	}
	if _, err := w.get(ctx, first, int(last-first)+blockHeadroom); err != nil {
		return nil, err
	}
	var out []Cue
	for _, it := range items {
		cues, err := m.readBlock(ctx, w, it)
		if err != nil {
			return nil, err
		}
		out = append(out, cues...)
	}
	return out, nil
}

func validBlockStart(b []byte) bool {
	e, ok := ebml.Next(b, 0)
	return ok && !e.Unknown && (e.ID == ebml.IDSimpleBlock || e.ID == ebml.IDBlockGroup)
}

func (m *mkvFile) readBlock(ctx context.Context, w *window, it Item) ([]Cue, error) {
	ref := it.ref.(mkvRef)
	pos := it.Pos
	hdr, err := w.get(ctx, pos, 16)
	if err != nil {
		return nil, err
	}
	if !validBlockStart(hdr) {
		// The Cluster's ID and size field is not the length the first one had.
		n, err := m.clusterHeaderLen(ctx, w, ref.cue.Cluster)
		if err != nil {
			return nil, err
		}
		m.hdrGuess = n
		pos = m.segStart + ref.cue.Cluster + int64(n) + ref.cue.Rel
		if hdr, err = w.get(ctx, pos, 16); err != nil {
			return nil, err
		}
		if !validBlockStart(hdr) {
			return nil, fmt.Errorf("subextract: no block at %d", pos)
		}
	}
	e, _ := ebml.Next(hdr, 0)
	if e.Size > maxRead {
		return nil, fmt.Errorf("subextract: block of %d bytes at %d", e.Size, pos)
	}
	body, err := w.get(ctx, pos+int64(e.Start), int(e.Size))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) < e.Size {
		return nil, fmt.Errorf("subextract: block at %d is cut off", pos)
	}
	block, dur := body, ref.cue.Dur
	if e.ID == ebml.IDBlockGroup {
		block = nil
		ebml.Children(body, func(c ebml.Element, p []byte) bool {
			switch c.ID {
			case ebml.IDBlock:
				block = p
			case ebml.IDBlockDuration:
				dur = int64(ebml.ReadUint64(p))
			}
			return true
		})
		if block == nil {
			return nil, fmt.Errorf("subextract: block group at %d has no block", pos)
		}
	}
	return m.decodeBlock(ref, block, dur)
}

// decodeBlock turns the payload of a Block into cues, one per lace.
func (m *mkvFile) decodeBlock(ref mkvRef, block []byte, dur int64) ([]Cue, error) {
	tn, n, _, ok := ebml.ReadSize(block, 0)
	if !ok || len(block) < n+3 {
		return nil, errors.New("subextract: short block")
	}
	if int(tn) != ref.track.Number {
		return nil, fmt.Errorf("subextract: block of track %d where track %d was expected", tn, ref.track.Number)
	}
	flags := block[n+2]
	frames, err := parseLaces(block[n+3:], int(flags>>1)&3)
	if err != nil {
		return nil, err
	}
	if dur < 0 {
		dur = 0
		if ref.track.DefaultDur > 0 && m.scale > 0 {
			dur = ref.track.DefaultDur * int64(len(frames)) / m.scale
		}
	}
	var out []Cue
	t := ref.cue.Time
	for i, f := range frames {
		laceDur := dur*int64(i+1)/int64(len(frames)) - dur*int64(i)/int64(len(frames))
		data, err := ref.track.decode(f)
		if err != nil {
			return nil, err
		}
		start, end := m.ticksToMs(t), m.ticksToMs(t+laceDur)
		c := Cue{Track: ref.track.order, Start: start, End: end, PTS: int64(math.Round(float64(t) * float64(m.scale) * 9e-5))}
		switch mkvFormat(ref.track) {
		case FormatSRT:
			c.Text = trimNewlines(string(data))
		case FormatASS:
			ro, rest, ok := strings.Cut(string(data), ",")
			order, err := strconv.Atoi(ro)
			if !ok || err != nil {
				return nil, fmt.Errorf("subextract: bad ASS event %q", string(data))
			}
			c.Order, c.Text = order, trimNewlines(rest)
		case FormatVTT:
			var ok bool
			if c.ID, c.Settings, c.Text, ok = parseWebVTTBlock(data); !ok {
				t += laceDur
				continue
			}
		case FormatSUP:
			c.Data = data
		}
		out = append(out, c)
		t += laceDur
	}
	return out, nil
}

func trimNewlines(s string) string { return strings.TrimRight(s, "\r\n") }

// parseWebVTTBlock splits a Matroska WebVTT block, "identifier\nsettings\ntext"
// (FFmpeg matroska_parse_webvtt).
func parseWebVTTBlock(b []byte) (id, settings, text string, ok bool) {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	id, rest, ok1 := strings.Cut(s, "\n")
	settings, text, ok2 := strings.Cut(rest, "\n")
	text = trimNewlines(text)
	if !ok1 || !ok2 || text == "" {
		return "", "", "", false
	}
	return id, settings, text, true
}

// parseLaces splits the payload of a Block into frames (RFC 9559 §10.3,
// FFmpeg matroska_parse_laces).
func parseLaces(b []byte, lacing int) ([][]byte, error) {
	if lacing == 0 {
		return [][]byte{b}, nil
	}
	if len(b) < 1 {
		return nil, errors.New("subextract: short laced block")
	}
	count := int(b[0]) + 1
	b = b[1:]
	sizes := make([]int, count)
	switch lacing {
	case 1: // Xiph
		total := 0
		for i := 0; i < count-1; i++ {
			for {
				if len(b) == 0 {
					return nil, errors.New("subextract: short Xiph lacing")
				}
				v := int(b[0])
				b = b[1:]
				sizes[i] += v
				total += v
				if v != 0xFF {
					break
				}
			}
		}
		if len(b) < total {
			return nil, errors.New("subextract: short Xiph lacing")
		}
		sizes[count-1] = len(b) - total
	case 2: // fixed
		if len(b)%count != 0 {
			return nil, errors.New("subextract: fixed lacing does not divide the payload")
		}
		for i := range sizes {
			sizes[i] = len(b) / count
		}
	case 3: // EBML
		first, n, _, ok := ebml.ReadSize(b, 0)
		if !ok {
			return nil, errors.New("subextract: short EBML lacing")
		}
		sizes[0] = int(first)
		total := sizes[0]
		pos := n
		for i := 1; i < count-1; i++ {
			v, n, _, ok := ebml.ReadSize(b, pos)
			if !ok {
				return nil, errors.New("subextract: short EBML lacing")
			}
			// signed difference: the value less half the range of its length
			diff := v - (int64(1)<<(7*uint(n)-1) - 1)
			sizes[i] = sizes[i-1] + int(diff)
			if sizes[i] < 0 {
				return nil, errors.New("subextract: negative lace size")
			}
			total += sizes[i]
			pos += n
		}
		b = b[pos:]
		if len(b) < total {
			return nil, errors.New("subextract: short EBML lacing")
		}
		sizes[count-1] = len(b) - total
	}
	out := make([][]byte, count)
	for i, s := range sizes {
		if s < 0 || s > len(b) {
			return nil, errors.New("subextract: lace overruns the block")
		}
		out[i], b = b[:s], b[s:]
	}
	return out, nil
}

// decode undoes the track's content encodings on one frame: zlib, bzlib and
// header stripping, as matroska_decode_buffer does.
func (t *mkvTrack) decode(b []byte) ([]byte, error) {
	for _, e := range t.enc {
		if e.scope&1 == 0 {
			continue
		}
		switch e.algo {
		case 0:
			r, err := zlib.NewReader(bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			if b, err = io.ReadAll(r); err != nil {
				return nil, err
			}
		case 1:
			var err error
			if b, err = io.ReadAll(bzip2.NewReader(bytes.NewReader(b))); err != nil {
				return nil, err
			}
		case 3:
			b = append(append([]byte(nil), e.settings...), b...)
		default:
			return nil, fmt.Errorf("subextract: content compression %d is not supported", e.algo)
		}
	}
	return b, nil
}
