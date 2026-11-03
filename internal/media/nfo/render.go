package nfo

import (
	"encoding/xml"
	"strconv"
)

type outID struct {
	Type    string `xml:"type,attr"`
	Default bool   `xml:"default,attr,omitempty"`
	Value   string `xml:",chardata"`
}

type outDoc struct {
	XMLName       xml.Name
	Title         string  `xml:"title,omitempty"`
	OriginalTitle string  `xml:"originaltitle,omitempty"`
	Year          int     `xml:"year,omitempty"`
	UniqueIDs     []outID `xml:"uniqueid"`
}

// Render writes the smallest NFO that Kodi and Jellyfin read back to the same
// work: the title, the original title, the year and the provider ids
// (<uniqueid type="tmdb" default="true"> first, then imdb). The root element is
// <movie> or <tvshow> by i.Kind ("movie" or "tv"). The ids of i must be set.
func Render(i Info) ([]byte, error) {
	root := "tvshow"
	if i.Kind == "movie" {
		root = "movie"
	}
	d := outDoc{XMLName: xml.Name{Local: root}, Title: i.Title, OriginalTitle: i.OriginalTitle, Year: i.Year}
	if i.TMDB != "" {
		d.UniqueIDs = append(d.UniqueIDs, outID{Type: "tmdb", Default: true, Value: i.TMDB})
	}
	if i.IMDb != "" {
		d.UniqueIDs = append(d.UniqueIDs, outID{Type: "imdb", Default: i.TMDB == "", Value: i.IMDb})
	}
	body, err := xml.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return []byte(xml.Header + string(body) + "\n"), nil
}

// FileName is the name of the NFO file for a work: tvshow.nfo or movie.nfo in
// its folder, or the video's name with .nfo for a single file (the first path
// Jellyfin's savers use, so it reads back what it wrote).
func FileName(isDir bool, kind, videoName string) string {
	switch {
	case !isDir:
		return Candidates(false, videoName, "", kind)[0]
	case kind == "movie":
		return "movie.nfo"
	}
	return "tvshow.nfo"
}

// YearOf is the year of a "YYYY-..." date, 0 when there is none.
func YearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	n, _ := strconv.Atoi(date[:4])
	return n
}
