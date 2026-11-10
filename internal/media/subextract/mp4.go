package subextract

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"

	mp4 "github.com/abema/go-mp4"
	log "github.com/sirupsen/logrus"
)

// mp4Track is one subtitle track of an MP4/MOV file with its sample table
// expanded: where every sample is, how big, and when.
type mp4Track struct {
	order    int
	sample   string // sample entry type: tx3g, wvtt
	lang     string
	samples  []mp4Sample
	fragment bool
}

type mp4Sample struct {
	pos, size int64
	startMs   int64
	durMs     int64
}

type mp4File struct {
	subs []*mp4Track
}

// openMP4 finds the moov box and reads the sample tables of its subtitle
// tracks. go-mp4 parses the boxes; it is handed the moov box alone, whose chunk
// offsets are file offsets all the same.
func openMP4(ctx context.Context, src Source) (*mp4File, error) {
	w := newWindow(src, 4096)
	moovOff, moovSize := int64(-1), int64(0)
	for pos := int64(0); pos < src.Size(); {
		b, err := w.get(ctx, pos, 16)
		if err != nil {
			return nil, err
		}
		if len(b) < 8 {
			break
		}
		size := int64(binary.BigEndian.Uint32(b))
		typ := string(b[4:8])
		hdr := int64(8)
		switch size {
		case 0:
			size = src.Size() - pos
		case 1:
			if len(b) < 16 {
				return nil, errors.New("subextract: truncated box header")
			}
			size, hdr = int64(binary.BigEndian.Uint64(b[8:])), 16
		}
		if size < hdr {
			return nil, errors.New("subextract: bad box size")
		}
		if typ == "moov" {
			moovOff, moovSize = pos, size
			break
		}
		pos += size
	}
	if moovOff < 0 {
		return nil, errors.New("subextract: no moov box")
	}
	if moovSize > maxRead {
		return nil, fmt.Errorf("subextract: moov of %d bytes", moovSize)
	}
	moov, err := w.get(ctx, moovOff, int(moovSize))
	if err != nil {
		return nil, err
	}
	if int64(len(moov)) < moovSize {
		return nil, errors.New("subextract: moov is cut off")
	}
	return parseMoov(bytes.NewReader(moov))
}

func parseMoov(r io.ReadSeeker) (*mp4File, error) {
	m := &mp4File{}
	fragmented := false
	if mvex, err := mp4.ExtractBox(r, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeMvex()}); err == nil && len(mvex) > 0 {
		fragmented = true
	}
	traks, err := mp4.ExtractBox(r, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()})
	if err != nil {
		return nil, err
	}
	chapters := map[uint32]bool{}
	type found struct {
		bi *mp4.BoxInfo
		id uint32
	}
	var subs []found
	for _, trak := range traks {
		hdlr, err := payloadAt[*mp4.Hdlr](r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()})
		if err != nil {
			return nil, err
		}
		tkhd, err := payloadAt[*mp4.Tkhd](r, trak, mp4.BoxPath{mp4.BoxTypeTkhd()})
		if err != nil {
			return nil, err
		}
		for _, id := range chapterRefs(r, trak) {
			chapters[id] = true
		}
		if hdlr == nil || tkhd == nil {
			continue
		}
		switch string(hdlr.HandlerType[:]) {
		case "sbtl", "subt", "text":
			subs = append(subs, found{trak, tkhd.TrackID})
		}
	}
	for _, f := range subs {
		if chapters[f.id] {
			continue // a chapter title track is not a subtitle
		}
		t, err := readSubtitleTrack(r, f.bi)
		if err != nil {
			return nil, err
		}
		if t == nil {
			continue
		}
		t.fragment = fragmented
		t.order = len(m.subs)
		m.subs = append(m.subs, t)
	}
	return m, nil
}

// payloadAt returns the payload of the first box at path under parent, or the
// zero value when there is none.
func payloadAt[T mp4.IBox](r io.ReadSeeker, parent *mp4.BoxInfo, path mp4.BoxPath) (T, error) {
	var zero T
	bs, err := mp4.ExtractBoxWithPayload(r, parent, path)
	if err != nil {
		return zero, err
	}
	if len(bs) == 0 {
		return zero, nil
	}
	v, _ := bs[0].Payload.(T)
	return v, nil
}

// chapterRefs lists the track ids a trak's tref/chap box points at.
func chapterRefs(r io.ReadSeeker, trak *mp4.BoxInfo) []uint32 {
	trefs, err := mp4.ExtractBox(r, trak, mp4.BoxPath{mp4.StrToBoxType("tref")})
	if err != nil || len(trefs) == 0 {
		return nil
	}
	if _, err := trefs[0].SeekToPayload(r); err != nil {
		return nil
	}
	payload := make([]byte, trefs[0].Size-trefs[0].HeaderSize)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil
	}
	var ids []uint32
	for len(payload) >= 8 {
		size := int(binary.BigEndian.Uint32(payload))
		if size < 8 || size > len(payload) {
			break
		}
		if string(payload[4:8]) == "chap" {
			for b := payload[8:size]; len(b) >= 4; b = b[4:] {
				ids = append(ids, binary.BigEndian.Uint32(b))
			}
		}
		payload = payload[size:]
	}
	return ids
}

// readSubtitleTrack expands the sample table of a subtitle trak; nil for one
// whose sample entry is not a text format this reads.
func readSubtitleTrack(r io.ReadSeeker, trak *mp4.BoxInfo) (*mp4Track, error) {
	stbl := mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl()}
	path := func(t mp4.BoxType) mp4.BoxPath { return append(append(mp4.BoxPath{}, stbl...), t) }

	stsd, err := mp4.ExtractBox(r, trak, path(mp4.BoxTypeStsd()))
	if err != nil || len(stsd) == 0 {
		return nil, err
	}
	entries, err := mp4.ExtractBox(r, stsd[0], mp4.BoxPath{mp4.BoxTypeAny()})
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	t := &mp4Track{sample: entries[0].Type.String()}
	if t.sample != "tx3g" && t.sample != "wvtt" {
		return nil, nil
	}
	mdhd, err := payloadAt[*mp4.Mdhd](r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()})
	if err != nil || mdhd == nil || mdhd.Timescale == 0 {
		return nil, err
	}
	t.lang = mdhdLanguage(mdhd.Language)
	stts, err := payloadAt[*mp4.Stts](r, trak, path(mp4.BoxTypeStts()))
	if err != nil {
		return nil, err
	}
	stsc, err := payloadAt[*mp4.Stsc](r, trak, path(mp4.BoxTypeStsc()))
	if err != nil {
		return nil, err
	}
	stsz, err := payloadAt[*mp4.Stsz](r, trak, path(mp4.BoxTypeStsz()))
	if err != nil {
		return nil, err
	}
	var offsets []uint64
	if stco, err := payloadAt[*mp4.Stco](r, trak, path(mp4.BoxTypeStco())); err != nil {
		return nil, err
	} else if stco != nil {
		for _, o := range stco.ChunkOffset {
			offsets = append(offsets, uint64(o))
		}
	} else if co64, err := payloadAt[*mp4.Co64](r, trak, path(mp4.BoxTypeCo64())); err != nil {
		return nil, err
	} else if co64 != nil {
		offsets = co64.ChunkOffset
	}
	if stts == nil || stsc == nil || stsz == nil {
		return t, nil // no samples: a fragmented file's track
	}

	// Sample sizes and times.
	var sizes []int64
	if stsz.SampleSize != 0 {
		for i := uint32(0); i < stsz.SampleCount; i++ {
			sizes = append(sizes, int64(stsz.SampleSize))
		}
	} else {
		for _, s := range stsz.EntrySize {
			sizes = append(sizes, int64(s))
		}
	}
	scale := int64(mdhd.Timescale)
	var deltas []int64
	for _, e := range stts.Entries {
		for i := uint32(0); i < e.SampleCount; i++ {
			deltas = append(deltas, int64(e.SampleDelta))
		}
	}
	// Positions: samples fill the chunks in order, as stsc says.
	pos := make([]int64, 0, len(sizes))
	for ci, off := range offsets {
		per := samplesPerChunk(stsc, ci+1)
		at := int64(off)
		for k := uint32(0); k < per && len(pos) < len(sizes); k++ {
			pos = append(pos, at)
			at += sizes[len(pos)-1]
		}
	}
	var ticks int64
	for i := 0; i < len(pos) && i < len(deltas); i++ {
		t.samples = append(t.samples, mp4Sample{
			pos:     pos[i],
			size:    sizes[i],
			startMs: ticks * 1000 / scale,
			durMs:   (ticks+deltas[i])*1000/scale - ticks*1000/scale,
		})
		ticks += deltas[i]
	}
	return t, nil
}

// samplesPerChunk is stsc's answer for the 1-based chunk number.
func samplesPerChunk(stsc *mp4.Stsc, chunk int) uint32 {
	var per uint32
	for _, e := range stsc.Entries {
		if int(e.FirstChunk) > chunk {
			break
		}
		per = e.SamplesPerChunk
	}
	return per
}

func mdhdLanguage(l [3]byte) string {
	out := make([]byte, 3)
	for i, c := range l {
		if c < 0x20 {
			c += 0x60
		}
		out[i] = c
	}
	if string(out) == "und" {
		return "und"
	}
	return string(out)
}

func (m *mp4File) Tracks() []TrackInfo {
	out := make([]TrackInfo, len(m.subs))
	for i, t := range m.subs {
		f := FormatSRT
		if t.sample == "wvtt" {
			f = FormatVTT
		}
		out[i] = TrackInfo{
			Order: i, Codec: t.sample, Language: t.lang, Format: f,
			// A fragmented file keeps its samples in moof boxes, which are not read.
			Supported: !t.fragment && len(t.samples) > 0,
		}
		if !out[i].Supported {
			out[i].Format = ""
		}
	}
	return out
}

func (m *mp4File) Header(order int) []byte {
	if m.subs[order].sample == "wvtt" {
		return []byte("WEBVTT")
	}
	return nil
}

func (m *mp4File) Items(order int) ([]Item, error) {
	t := m.subs[order]
	items := make([]Item, 0, len(t.samples))
	for i, s := range t.samples {
		items = append(items, Item{Track: order, Time: s.startMs, Pos: s.pos, Size: s.size, ref: i})
	}
	return items, nil
}

func (m *mp4File) Fetch(ctx context.Context, w *window, items []Item) ([]Cue, []Item, error) {
	if len(items) == 0 {
		return nil, nil, nil
	}
	first, end := items[0].Pos, items[0].Pos+items[0].Size
	for _, it := range items {
		if it.Pos < first {
			first = it.Pos
		}
		if e := it.Pos + it.Size; e > end {
			end = e
		}
	}
	if _, err := w.get(ctx, first, int(end-first)); err != nil {
		return nil, nil, err
	}
	var out []Cue
	var failed []Item
	for _, it := range items {
		t := m.subs[it.Track]
		s := t.samples[it.ref.(int)]
		data, err := w.get(ctx, it.Pos, int(it.Size))
		if err != nil {
			return nil, nil, err
		}
		if int64(len(data)) < it.Size {
			log.Warnf("subextract: %v", fmt.Errorf("sample at %d is cut off", it.Pos))
			failed = append(failed, it)
			continue
		}
		c := Cue{Track: it.Track, Start: s.startMs, End: s.startMs + s.durMs, PTS: s.startMs * 90}
		switch t.sample {
		case "tx3g":
			text, ok := movTextSample(data)
			if !ok {
				continue
			}
			c.Text = text
		case "wvtt":
			var ok bool
			if c.ID, c.Settings, c.Text, ok = wvttSample(data); !ok {
				continue
			}
		}
		out = append(out, c)
	}
	return out, failed, nil
}

// movTextSample is the text of a tx3g sample: a 16-bit length, then that many
// bytes of UTF-8 (or UTF-16 with a byte order mark), then style boxes, which are
// not carried over (FFmpeg movtextdec.c). An empty sample is a gap.
func movTextSample(b []byte) (string, bool) {
	if len(b) < 2 {
		return "", false
	}
	n := int(binary.BigEndian.Uint16(b))
	if n == 0 || 2+n > len(b) {
		return "", false
	}
	text := b[2 : 2+n]
	if len(text) >= 2 && (text[0] == 0xFE && text[1] == 0xFF || text[0] == 0xFF && text[1] == 0xFE) {
		var u []uint16
		le := text[0] == 0xFF
		for i := 2; i+1 < len(text); i += 2 {
			if le {
				u = append(u, binary.LittleEndian.Uint16(text[i:]))
			} else {
				u = append(u, binary.BigEndian.Uint16(text[i:]))
			}
		}
		s := trimNewlines(string(utf16.Decode(u)))
		return s, s != ""
	}
	s := trimNewlines(string(text))
	return s, s != ""
}

// wvttSample reads the boxes of a WebVTT sample: vttc holds payl (the text),
// sttg (the settings) and iden (the identifier); vtte is an empty cue.
func wvttSample(b []byte) (id, settings, text string, ok bool) {
	for len(b) >= 8 {
		size := int(binary.BigEndian.Uint32(b))
		if size < 8 || size > len(b) {
			break
		}
		if string(b[4:8]) == "vttc" {
			inner := b[8:size]
			for len(inner) >= 8 {
				isz := int(binary.BigEndian.Uint32(inner))
				if isz < 8 || isz > len(inner) {
					break
				}
				body := string(inner[8:isz])
				switch string(inner[4:8]) {
				case "payl":
					text = trimNewlines(body)
				case "sttg":
					settings = body
				case "iden":
					id = body
				}
				inner = inner[isz:]
			}
		}
		b = b[size:]
	}
	return id, settings, text, text != ""
}
