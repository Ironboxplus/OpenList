package subextract

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// Render writes cues as a complete file of the given format. Called on partial
// sets as well: the result is always a valid file of what is known so far.
func Render(format string, header []byte, cues []Cue) []byte {
	switch format {
	case FormatSRT:
		return renderSRT(cues)
	case FormatASS:
		return renderASS(header, cues)
	case FormatVTT:
		return renderVTT(header, cues)
	case FormatSUP:
		return renderSUP(cues)
	}
	return nil
}

func sortedByStart(cues []Cue) []Cue {
	out := append([]Cue(nil), cues...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

func clock(ms int64, sep byte) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d%c%03d", ms/3600000, ms/60000%60, ms/1000%60, sep, ms%1000)
}

func renderSRT(cues []Cue) []byte {
	var b bytes.Buffer
	for i, c := range sortedByStart(cues) {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, clock(c.Start, ','), clock(c.End, ','), c.Text)
	}
	return b.Bytes()
}

func renderVTT(header []byte, cues []Cue) []byte {
	var b bytes.Buffer
	h := strings.TrimRight(strings.ReplaceAll(string(header), "\r\n", "\n"), "\n")
	if !strings.HasPrefix(h, "WEBVTT") {
		h = "WEBVTT"
	}
	b.WriteString(h + "\n\n")
	for _, c := range sortedByStart(cues) {
		if c.ID != "" {
			b.WriteString(c.ID + "\n")
		}
		fmt.Fprintf(&b, "%s --> %s", clock(c.Start, '.'), clock(c.End, '.'))
		if c.Settings != "" {
			b.WriteString(" " + c.Settings)
		}
		fmt.Fprintf(&b, "\n%s\n\n", c.Text)
	}
	return b.Bytes()
}

// assClock is H:MM:SS.cc from centiseconds, as assenc.c writes it.
func assClock(cs int64) string {
	if cs < 0 {
		cs = 0
	}
	return fmt.Sprintf("%d:%02d:%02d.%02d", cs/360000, cs/6000%60, cs/100%60, cs%100)
}

// renderASS is the script header from the track's CodecPrivate followed by the
// events in ReadOrder, each with its times rebuilt from the block (assenc.c
// write_packet: start and duration are rounded to centiseconds separately).
func renderASS(header []byte, cues []Cue) []byte {
	var b bytes.Buffer
	h := strings.ReplaceAll(string(header), "\r\n", "\n")
	b.WriteString(h)
	if h != "" && !strings.HasSuffix(h, "\n") {
		b.WriteByte('\n')
	}
	if !strings.Contains(h, "[Events]") {
		b.WriteString("[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")
	}
	ordered := append([]Cue(nil), cues...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })
	for _, c := range ordered {
		layer, rest, _ := strings.Cut(c.Text, ",")
		startCS := (c.Start + 5) / 10
		endCS := startCS + (c.End-c.Start+5)/10
		fmt.Fprintf(&b, "Dialogue: %s,%s,%s,%s\n", layer, assClock(startCS), assClock(endCS), rest)
	}
	return b.Bytes()
}

// renderSUP is a .sup stream: every segment of every block behind a "PG" header
// with the block's time in 90 kHz ticks as pts and dts (supenc.c).
func renderSUP(cues []Cue) []byte {
	var b bytes.Buffer
	ordered := append([]Cue(nil), cues...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].PTS < ordered[j].PTS })
	for _, c := range ordered {
		data := c.Data
		for len(data) > 2 {
			n := int(binary.BigEndian.Uint16(data[1:3])) + 3
			if n > len(data) {
				break
			}
			b.WriteString("PG")
			_ = binary.Write(&b, binary.BigEndian, uint32(c.PTS))
			_ = binary.Write(&b, binary.BigEndian, uint32(c.PTS))
			b.Write(data[:n])
			data = data[n:]
		}
	}
	return b.Bytes()
}
