// Package nfo reads the Kodi/Jellyfin NFO file that sits next to a work and
// writes a minimal one. Only what identifies the work is read: the provider ids,
// the title and the year.
//
// The formats follow Jellyfin's MediaBrowser.XbmcMetadata (the parser reads
// <uniqueid type="tmdb">, <tmdbid>, <imdbid>, and a file that is only a URL or
// has a URL after the closing tag still gives its ids: BaseNfoParser.cs
// ParseProviderLinks; the paths are those of MovieNfoSaver.GetMovieSavePaths
// and SeriesNfoSaver: movie.nfo, <video>.nfo, tvshow.nfo) and Kodi, where a file
// with ids and no details is a "URL" result that the scraper then fills
// (xbmc/video/VideoInfoScanner.cpp, InfoType::URL).
package nfo

import (
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// MaxSize is how much of an NFO file is read.
const MaxSize = 64 << 10

// Info is what an NFO says about a work.
type Info struct {
	Kind          string // "movie" or "tv" when the root element or a URL says so, else ""
	TMDB          string // digits
	IMDb          string // tt...
	Title         string
	OriginalTitle string
	Year          int
}

// HasID reports whether the file names a work at a provider.
func (i Info) HasID() bool { return i.TMDB != "" || i.IMDb != "" }

type uniqueID struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type doc struct {
	XMLName       xml.Name
	Title         string     `xml:"title"`
	OriginalTitle string     `xml:"originaltitle"`
	Year          string     `xml:"year"`
	Premiered     string     `xml:"premiered"`
	UniqueIDs     []uniqueID `xml:"uniqueid"`
	TMDBID        string     `xml:"tmdbid"`
	TMDB          string     `xml:"tmdb"`
	IMDbID        string     `xml:"imdbid"`
	IMDb          string     `xml:"imdb"`
	ID            string     `xml:"id"`
}

var (
	tmdbURLRe = regexp.MustCompile(`themoviedb\.org/(movie|tv)/(\d+)`)
	imdbURLRe = regexp.MustCompile(`imdb\.com/title/(tt\d+)`)
	imdbIDRe  = regexp.MustCompile(`^tt\d{5,}$`)
	digitsRe  = regexp.MustCompile(`^\d+$`)
)

// Parse reads an NFO. XML that does not parse still gives the ids of the URLs in
// it; an error is returned only when the file has neither.
func Parse(data []byte) (Info, error) {
	if len(data) > MaxSize {
		data = data[:MaxSize]
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var info Info
	var d doc
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil } // UTF-8 assumed
	xmlErr := dec.Decode(&d)
	if xmlErr == nil {
		switch strings.ToLower(d.XMLName.Local) {
		case "movie":
			info.Kind = "movie"
		case "tvshow":
			info.Kind = "tv"
		}
		info.Title = strings.TrimSpace(d.Title)
		info.OriginalTitle = strings.TrimSpace(d.OriginalTitle)
		info.Year, _ = strconv.Atoi(strings.TrimSpace(d.Year))
		if info.Year == 0 && len(d.Premiered) >= 4 {
			info.Year, _ = strconv.Atoi(d.Premiered[:4])
		}
		setTMDB := func(v string) {
			if v = strings.TrimSpace(v); info.TMDB == "" && digitsRe.MatchString(v) {
				info.TMDB = v
			}
		}
		setIMDb := func(v string) {
			if v = strings.TrimSpace(v); info.IMDb == "" && imdbIDRe.MatchString(v) {
				info.IMDb = v
			}
		}
		for _, u := range d.UniqueIDs {
			switch strings.ToLower(strings.TrimSpace(u.Type)) {
			case "tmdb", "themoviedb":
				setTMDB(u.Value)
			case "imdb":
				setIMDb(u.Value)
			}
		}
		setTMDB(d.TMDBID)
		setTMDB(d.TMDB)
		setIMDb(d.IMDbID)
		setIMDb(d.IMDb)
		setIMDb(d.ID) // Kodi's older <id>tt0133093</id>
	}
	// a link anywhere in the file (a file that is only a URL, or one after the XML)
	if m := tmdbURLRe.FindSubmatch(data); m != nil {
		if info.TMDB == "" {
			info.TMDB = string(m[2])
		}
		if info.Kind == "" {
			info.Kind = string(m[1])
		}
	}
	if m := imdbURLRe.FindSubmatch(data); m != nil && info.IMDb == "" {
		info.IMDb = string(m[1])
	}
	if !info.HasID() && xmlErr != nil {
		return Info{}, xmlErr
	}
	return info, nil
}

// Candidates are the NFO file names to look for next to a work, best first.
// For a folder work: tvshow.nfo (a show), movie.nfo (a film), the main video's
// name, the folder's own name; for a single file: the file's name with .nfo
// (Jellyfin's MovieNfoSaver.GetMovieSavePaths). video is the name of the main
// video inside a folder work, "" when unknown.
func Candidates(isDir bool, name, video, kind string) []string {
	stem := func(s string) string { return strings.TrimSuffix(s, path.Ext(s)) + ".nfo" }
	if !isDir {
		return []string{stem(name)}
	}
	var out []string
	if kind == "movie" {
		out = append(out, "movie.nfo")
	} else {
		out = append(out, "tvshow.nfo", "movie.nfo")
	}
	if video != "" {
		out = append(out, stem(video))
	}
	return append(out, name+".nfo")
}

// Find returns the entry of names that matches the first candidate that is there
// (case-insensitive), or "".
func Find(names []string, candidates []string) string {
	for _, c := range candidates {
		for _, n := range names {
			if strings.EqualFold(n, c) {
				return n
			}
		}
	}
	return ""
}
