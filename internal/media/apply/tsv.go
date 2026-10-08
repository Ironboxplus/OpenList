package apply

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Row is one line of a plan file. Every column may be edited by hand.
type Row struct {
	Path       string
	Kind       string
	Provider   string
	ID         string
	Title      string
	Year       int
	Confidence float32
	Note       string
}

const tsvHeader = "#path\tkind\tprovider\tid\ttitle\tyear\tconfidence\tnote"

func clean(s string) string {
	s = strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(s)
	return strings.TrimSpace(s)
}

// FormatTSV writes rows in the plan format: path, kind, provider, id, title,
// year, confidence, note, tab separated, with a comment header.
func FormatTSV(w io.Writer, rows []Row) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, tsvHeader)
	for _, r := range rows {
		year := ""
		if r.Year > 0 {
			year = strconv.Itoa(r.Year)
		}
		fmt.Fprintf(bw, "%s\t%s\t%s\t%s\t%s\t%s\t%.2f\t%s\n",
			clean(r.Path), clean(r.Kind), clean(r.Provider), clean(r.ID), clean(r.Title), year, r.Confidence, clean(r.Note))
	}
	return bw.Flush()
}

// ParseTSV reads a plan file. Blank lines and lines starting with # are
// skipped; missing trailing columns are empty.
func ParseTSV(r io.Reader) ([]Row, error) {
	var rows []Row
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#") {
			continue
		}
		f := strings.Split(text, "\t")
		if len(f) < 2 {
			return nil, fmt.Errorf("line %d: need at least path and kind", line)
		}
		for len(f) < 8 {
			f = append(f, "")
		}
		row := Row{Path: strings.TrimSpace(f[0]), Kind: strings.TrimSpace(f[1]), Provider: strings.TrimSpace(f[2]),
			ID: strings.TrimSpace(f[3]), Title: strings.TrimSpace(f[4]), Note: strings.TrimSpace(f[7])}
		if !strings.HasPrefix(row.Path, "/") {
			return nil, fmt.Errorf("line %d: path %q is not absolute", line, row.Path)
		}
		switch row.Kind {
		case "movie", "tv", "anime", "collection", "ignore", "library":
		default:
			return nil, fmt.Errorf("line %d: unknown kind %q", line, row.Kind)
		}
		if y := strings.TrimSpace(f[5]); y != "" {
			n, err := strconv.Atoi(y)
			if err != nil {
				return nil, fmt.Errorf("line %d: bad year %q", line, y)
			}
			row.Year = n
		}
		if c := strings.TrimSpace(f[6]); c != "" {
			v, err := strconv.ParseFloat(c, 32)
			if err != nil {
				return nil, fmt.Errorf("line %d: bad confidence %q", line, c)
			}
			row.Confidence = float32(v)
		}
		if row.Provider != "" && row.ID == "" {
			return nil, fmt.Errorf("line %d: provider %q without an id", line, row.Provider)
		}
		rows = append(rows, row)
	}
	return rows, sc.Err()
}
