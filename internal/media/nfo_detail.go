package media

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/media/nfo"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

var fullDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// IsScraped reports whether something outside the file name gave a work its
// data: a scrape ran, a provider id is known, or a person fixed it.
func IsScraped(item *model.MediaItem) bool {
	return item.ScrapedAt != nil || item.ExternalIDs != "" || item.Locked || item.Source == model.MediaSourceManual
}

// NFODetail is what the NFO of a work (or of the show an episode belongs to)
// says: the ids and titles, and what the scrape knows of it. Unlike the NFO
// that ExportNFO writes next to a work, it does not depend on the work being
// locked or picked by hand.
func NFODetail(item *model.MediaItem) nfo.Detail {
	var ids map[string]string
	if item.ExternalIDs != "" {
		_ = json.Unmarshal([]byte(item.ExternalIDs), &ids)
	}
	d := nfo.Detail{Info: nfo.Info{
		Title: itemTitle(item), OriginalTitle: item.OriginalTitle, Year: nfo.YearOf(item.ReleaseDate), IMDb: ids["imdb"],
	}}
	if typ, id, ok := strings.Cut(ids["tmdb"], "/"); ok {
		d.Kind, d.TMDB = map[string]string{"movie": "movie", "tv": "tv"}[typ], id
	}
	if d.Kind == "" {
		d.Kind = "tv"
		if !item.IsSeries() {
			d.Kind = "movie"
		}
	}
	d.Plot = item.Plot
	if d.Plot == "" {
		d.Plot = item.Description
	}
	d.Genres = splitGenres(item.Genre)
	d.Rating = item.Rating
	if fullDateRe.MatchString(item.ReleaseDate) {
		d.Premiered = item.ReleaseDate
	}
	d.Actors = actorsOf(item.Authors)
	return d
}

// EpisodeNFODetail is the <episodedetails> of one episode of a work: title and
// overview from its media_episodes row (ep may be nil), numbered season, num.
func EpisodeNFODetail(item *model.MediaItem, ep *model.MediaEpisode, season, num int) nfo.Detail {
	d := nfo.Detail{Root: nfo.RootEpisode, ShowTitle: itemTitle(item), Season: season, Episode: num}
	if ep != nil {
		d.Title, d.Plot = strings.TrimSpace(ep.Title), ep.Overview
	}
	return d
}

func splitGenres(s string) []string {
	var out []string
	for _, g := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '，' || r == '、' || r == '/' }) {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// actorsOf reads MediaItem.Authors: a JSON array of names, or of {name, role}.
func actorsOf(authors string) []nfo.Actor {
	if authors == "" {
		return nil
	}
	var names []string
	if json.Unmarshal([]byte(authors), &names) == nil {
		out := make([]nfo.Actor, 0, len(names))
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" {
				out = append(out, nfo.Actor{Name: n})
			}
		}
		return out
	}
	var objs []struct {
		Name      string `json:"name"`
		Role      string `json:"role"`
		Character string `json:"character"`
	}
	if json.Unmarshal([]byte(authors), &objs) != nil {
		return nil
	}
	out := make([]nfo.Actor, 0, len(objs))
	for _, o := range objs {
		if n := strings.TrimSpace(o.Name); n != "" {
			role := o.Role
			if role == "" {
				role = o.Character
			}
			out = append(out, nfo.Actor{Name: n, Role: role})
		}
	}
	return out
}
