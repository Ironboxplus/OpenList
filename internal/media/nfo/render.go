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

type outActor struct {
	Name string `xml:"name"`
	Role string `xml:"role,omitempty"`
}

type outDoc struct {
	XMLName       xml.Name
	Title         string     `xml:"title,omitempty"`
	OriginalTitle string     `xml:"originaltitle,omitempty"`
	ShowTitle     string     `xml:"showtitle,omitempty"`
	Year          int        `xml:"year,omitempty"`
	Plot          string     `xml:"plot,omitempty"`
	Genres        []string   `xml:"genre"`
	Rating        float32    `xml:"rating,omitempty"`
	Premiered     string     `xml:"premiered,omitempty"`
	ReleaseDate   string     `xml:"releasedate,omitempty"`
	Runtime       int        `xml:"runtime,omitempty"`
	Season        *int       `xml:"season"`
	Episode       *int       `xml:"episode"`
	SeasonNumber  *int       `xml:"seasonnumber"`
	Actors        []outActor `xml:"actor"`
	UniqueIDs     []outID    `xml:"uniqueid"`
}

// Root elements of the files Kodi and Jellyfin read.
const (
	RootMovie   = "movie"
	RootTVShow  = "tvshow"
	RootSeason  = "season"
	RootEpisode = "episodedetails"
)

// Actor is a cast member of a work.
type Actor struct {
	Name string
	Role string
}

// Detail is everything Render can write about a work: Info (the ids, the title
// and the year) plus the descriptive fields a media server shows. Zero fields
// are left out of the file.
type Detail struct {
	Info
	Root      string // RootMovie, RootTVShow, RootSeason or RootEpisode; "" = by Info.Kind
	ShowTitle string // episode: the show it belongs to
	Plot      string
	Genres    []string
	Rating    float32 // 0-10
	Premiered string  // YYYY-MM-DD
	Runtime   int     // minutes
	Actors    []Actor
	Season    int // season (RootSeason) or the season of an episode
	Episode   int // RootEpisode: the number in its season
}

// Render writes the smallest NFO that Kodi and Jellyfin read back to the same
// work: the title, the original title, the year and the provider ids
// (<uniqueid type="tmdb" default="true"> first, then imdb). The root element is
// <movie> or <tvshow> by i.Kind ("movie" or "tv"). The ids of i must be set.
func Render(i Info) ([]byte, error) {
	return RenderDetail(Detail{Info: i})
}

// RenderDetail writes d as an NFO: a <movie>, <tvshow>, <season> or
// <episodedetails> (d.Root, else by d.Kind) with the elements Kodi's and
// Jellyfin's readers know (MovieNfoParser / BaseNfoParser): title, plot,
// genre (one element each), rating, premiered, runtime in minutes, actor,
// season and episode numbers, uniqueid.
func RenderDetail(d Detail) ([]byte, error) {
	root := d.Root
	if root == "" {
		root = RootTVShow
		if d.Kind == "movie" {
			root = RootMovie
		}
	}
	o := outDoc{
		XMLName: xml.Name{Local: root}, Title: d.Title, OriginalTitle: d.OriginalTitle, ShowTitle: d.ShowTitle,
		Year: d.Year, Plot: d.Plot, Genres: d.Genres, Rating: d.Rating, Premiered: d.Premiered, Runtime: d.Runtime,
	}
	if root == RootMovie {
		o.ReleaseDate = d.Premiered // Jellyfin reads either; Kodi writes both
	}
	switch root {
	case RootEpisode:
		o.Season, o.Episode = &d.Season, &d.Episode
	case RootSeason:
		o.SeasonNumber = &d.Season
	}
	for _, a := range d.Actors {
		o.Actors = append(o.Actors, outActor{Name: a.Name, Role: a.Role})
	}
	if d.TMDB != "" {
		o.UniqueIDs = append(o.UniqueIDs, outID{Type: "tmdb", Default: true, Value: d.TMDB})
	}
	if d.IMDb != "" {
		o.UniqueIDs = append(o.UniqueIDs, outID{Type: "imdb", Default: d.TMDB == "", Value: d.IMDb})
	}
	body, err := xml.MarshalIndent(o, "", "  ")
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
