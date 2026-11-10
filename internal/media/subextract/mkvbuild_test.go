package subextract

import (
	"bytes"
	"encoding/binary"
)

// A small Matroska writer, for the files neither ffmpeg nor mkvmerge will make
// on request: laced subtitle blocks, header stripping, a Cluster whose size
// field is longer than its neighbours', a file with no Cues. The output is a
// real file, which ffmpeg reads for the reference.

func idBytes(id uint32) []byte {
	switch {
	case id > 0xFFFFFF:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	case id > 0xFF:
		return []byte{byte(id >> 8), byte(id)}
	}
	return []byte{byte(id)}
}

// sizeBytes writes n as an EBML size of width bytes.
func sizeBytes(n int, width int) []byte {
	b := make([]byte, width)
	v := uint64(n)
	for i := width - 1; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	b[0] |= 1 << (8 - uint(width))
	return b
}

func minWidth(n int) int {
	for w := 1; w < 8; w++ {
		if n < (1<<(7*uint(w)))-1 {
			return w
		}
	}
	return 8
}

// elem is an element with the smallest size field that holds its payload.
func elem(id uint32, payload ...[]byte) []byte { return elemW(id, 0, payload...) }

// elemW is an element whose size field is width bytes (0: the smallest).
func elemW(id uint32, width int, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	if width == 0 {
		width = minWidth(len(body))
	}
	return bytes.Join([][]byte{idBytes(id), sizeBytes(len(body), width), body}, nil)
}

func uintElem(id uint32, v uint64) []byte {
	var b []byte
	for x := v; ; x >>= 8 {
		b = append([]byte{byte(x)}, b...)
		if x < 256 {
			break
		}
	}
	return elem(id, b)
}

func uint8Elem(id uint32, v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return elem(id, b)
}

type bTrack struct {
	num     int
	codec   string
	name    string
	lang    string
	private []byte
	// stripped is a header every frame has cut off (ContentEncoding header
	// stripping); empty for none.
	stripped []byte
}

type bBlock struct {
	track  int
	ms     int64
	dur    int64
	lacing int // 0 none, 1 Xiph, 2 fixed, 3 EBML
	frames [][]byte
	// wideCluster gives the block's Cluster a size field of 8 bytes instead of 4.
	wideCluster bool
	// group puts the block in a BlockGroup (with its duration); false writes a
	// SimpleBlock.
	simple bool
}

func laceFrames(lacing int, frames [][]byte) []byte {
	if lacing == 0 {
		return frames[0]
	}
	out := []byte{byte(len(frames) - 1)}
	switch lacing {
	case 1:
		for _, f := range frames[:len(frames)-1] {
			n := len(f)
			for n >= 255 {
				out = append(out, 255)
				n -= 255
			}
			out = append(out, byte(n))
		}
	case 3:
		out = append(out, sizeBytes(len(frames[0]), minWidth(len(frames[0])))...)
		prev := len(frames[0])
		for _, f := range frames[1 : len(frames)-1] {
			diff := len(f) - prev
			w := 1
			for ; w < 8; w++ {
				half := (1 << (7*uint(w) - 1)) - 1
				if diff >= -half && diff <= half {
					break
				}
			}
			half := (1 << (7*uint(w) - 1)) - 1
			out = append(out, sizeBytes(diff+half, w)...)
			prev = len(f)
		}
	}
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

// buildMKV writes a file with the tracks and blocks, one Cluster for every
// clusterMs milliseconds, and (withCues) a Cues element with an entry for every
// block. The Segment's SeekHead says where Info, Tracks and Cues are.
func buildMKV(tracks []bTrack, blocks []bBlock, clusterMs int64, withCues bool) []byte {
	info := elem(0x1549A966, uintElem(0x2AD7B1, 1_000_000), elem(0x4D80, []byte("subextract test")), elem(0x5741, []byte("subextract test")))
	var tes [][]byte
	for _, t := range tracks {
		parts := [][]byte{uintElem(0xD7, uint64(t.num)), uintElem(0x73C5, uint64(t.num)), uintElem(0x83, 17), elem(0x86, []byte(t.codec)), elem(0x22B59C, []byte(t.lang))}
		if t.name != "" {
			parts = append(parts, elem(0x536E, []byte(t.name)))
		}
		if len(t.private) > 0 {
			parts = append(parts, elem(0x63A2, t.private))
		}
		if len(t.stripped) > 0 {
			parts = append(parts, elem(0x6D80, elem(0x6240,
				uintElem(0x5031, 0), uintElem(0x5032, 1), uintElem(0x5033, 0),
				elem(0x5034, uintElem(0x4254, 3), elem(0x4255, t.stripped)))))
		}
		tes = append(tes, elem(0xAE, parts...))
	}
	tracksEl := elem(0x1654AE6B, tes...)

	// Clusters, remembering where each block lands inside its Cluster.
	type placed struct {
		cluster int // index
		rel     int
	}
	var clusterEls [][]byte
	var wide []bool
	places := make([]placed, len(blocks))
	cur, curStart, curIdx := []byte(nil), int64(-1), -1
	flush := func() {
		if curIdx >= 0 {
			w := 4
			if wide[curIdx] {
				w = 8
			}
			clusterEls = append(clusterEls, elemW(0x1F43B675, w, cur))
		}
	}
	for i, b := range blocks {
		start := b.ms / clusterMs * clusterMs
		if curIdx < 0 || start != curStart {
			flush()
			curIdx++
			curStart = start
			cur = uintElem(0xE7, uint64(start))
			wide = append(wide, false)
		}
		if b.wideCluster {
			wide[curIdx] = true
		}
		places[i] = placed{curIdx, len(cur)}
		payload := append([]byte{0x80 | byte(b.track)}, 0, 0, byte(b.lacing<<1))
		binary.BigEndian.PutUint16(payload[1:], uint16(b.ms-start))
		payload = append(payload, laceFrames(b.lacing, b.frames)...)
		if b.simple {
			cur = append(cur, elem(0xA3, payload)...)
		} else {
			cur = append(cur, elem(0xA0, elem(0xA1, payload), uintElem(0x9B, uint64(b.dur)))...)
		}
	}
	flush()

	// Positions: Segment children start after the SeekHead, which has a fixed
	// size (three entries, 8-byte positions).
	seekEntry := func(id uint32, pos int) []byte {
		return elem(0x4DBB, elem(0x53AB, idBytes(id)), uint8Elem(0x53AC, uint64(pos)))
	}
	seekLen := len(elem(0x114D9B74, seekEntry(0x1549A966, 0), seekEntry(0x1654AE6B, 0), seekEntry(0x1C53BB6B, 0)))
	infoPos := seekLen
	tracksPos := infoPos + len(info)
	clustersPos := tracksPos + len(tracksEl)
	clusterPos := make([]int, len(clusterEls))
	at := clustersPos
	for i, c := range clusterEls {
		clusterPos[i] = at
		at += len(c)
	}
	cuesPos := at
	var cues []byte
	if withCues {
		var cps [][]byte
		for i, b := range blocks {
			cps = append(cps, elem(0xBB, uintElem(0xB3, uint64(b.ms)), elem(0xB7,
				uintElem(0xF7, uint64(b.track)),
				uintElem(0xF1, uint64(clusterPos[places[i].cluster])),
				uintElem(0xF0, uint64(places[i].rel)),
			)))
		}
		cues = elem(0x1C53BB6B, cps...)
	}

	seek := elem(0x114D9B74, seekEntry(0x1549A966, infoPos), seekEntry(0x1654AE6B, tracksPos), seekEntry(0x1C53BB6B, cuesPos))
	var seg bytes.Buffer
	seg.Write(seek)
	seg.Write(info)
	seg.Write(tracksEl)
	for _, c := range clusterEls {
		seg.Write(c)
	}
	seg.Write(cues)

	head := elem(0x1A45DFA3, uintElem(0x4286, 1), uintElem(0x42F7, 1), uintElem(0x42F2, 4), uintElem(0x42F3, 8), elem(0x4282, []byte("matroska")), uintElem(0x4287, 4), uintElem(0x4285, 2))
	return bytes.Join([][]byte{head, elemW(0x18538067, 8, seg.Bytes())}, nil)
}
