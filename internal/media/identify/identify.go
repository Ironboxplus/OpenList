// Package identify decides what a directory (or a loose video) in a library
// is: its kind, title, year and season. The name gives the title; the
// content, listed once, gives the kind. Names alone cannot tell a movie from
// a show ("行尸走肉" and "寄生虫" look the same), which is why the kind comes
// from what is inside.
package identify

import (
	"context"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
)

// Result is what the rules concluded about one item.
type Result struct {
	Kind       string // movie | tv | anime | collection | ignore
	Theatrical bool   // an anime movie (剧场版): look it up as a movie
	Title      string
	English    string
	Year       int
	Season     int
	Episodes   int     // announced episode count, 0 = unknown
	Confidence float32 // how sure the rules are about Kind
	Why        string
	Parsed     scraper.ParsedName // for building search terms
	Group      string
	TMDBID     string // inline id tag in the name (digits); wins over any search
	IMDbID     string
}

// Terms are the queries to send to a provider, best first.
func (r Result) Terms() []scraper.SearchTerm { return scraper.SearchTerms(r.Parsed) }

// Classifier turns a name and its listing into a Result. P1 has the rule
// implementation only; a model-backed one can be plugged in later behind the
// same interface (off by default).
type Classifier interface {
	Classify(ctx context.Context, name string, isDir bool, entries []Entry) (Result, error)
}

// Rules is the rule-based Classifier.
type Rules struct{}

// Classify implements Classifier.
func (Rules) Classify(_ context.Context, name string, isDir bool, entries []Entry) (Result, error) {
	return Classify(name, isDir, entries), nil
}

// Classify combines the name and, for a directory, its listing. Pass entries
// nil when the directory was not listed: the kind then rests on the name and
// the confidence says so.
func Classify(name string, isDir bool, entries []Entry) Result {
	ni := ParseName(name, !isDir)
	r := Result{
		Title: ni.Title, English: ni.English, Year: ni.Year, Season: ni.Season,
		Episodes: ni.Episodes, Parsed: ni.Parsed, Group: ni.Group, TMDBID: ni.TMDBID, IMDbID: ni.IMDbID,
	}

	var shape Shape
	switch {
	case !isDir:
		if ni.TVHint && !ni.Collection {
			shape = Shape{Kind: "tv", Confidence: 0.6, Why: "episode marker in a file name"}
		} else {
			shape = Shape{Kind: "movie", Confidence: 0.85, Why: "a single video file"}
		}
	case entries != nil:
		shape = ClassifyEntries(entries)
	}

	kind, conf, why := shape.Kind, shape.Confidence, shape.Why
	if kind == "" {
		// no content to go on: the name decides, with less certainty
		switch {
		case ni.Collection:
			kind, conf, why = "collection", 0.6, "collection word in the name"
		case ni.TVHint:
			kind, conf, why = "tv", 0.65, "season/episode marker in the name"
		case ni.Anime && ni.Episodes > 0:
			kind, conf, why = "tv", 0.65, "fansub name with an episode range"
		default:
			kind, conf = "movie", 0.4
			why = "nothing to go on but the name"
			if shape.Why != "" {
				why = shape.Why + "; " + why
			}
		}
	} else if kind == "movie" && ni.TVHint && shape.Confidence < 0.85 {
		kind, conf, why = "tv", 0.6, "episode markers in the name outweigh a weak movie shape"
	}
	// An ignore verdict from content stands; a name that says "OST" does too.
	if kind != "ignore" && ostRe.MatchString(name) {
		kind, conf, why = "ignore", 0.7, "soundtrack in the name"
	}

	if kind == "ignore" {
		r.Kind, r.Confidence, r.Why = kind, conf, why
		return r
	}
	if ni.Anime && (kind == "tv" || kind == "movie") {
		r.Theatrical = kind == "movie" || ni.Theatrical
		kind = "anime"
	}
	if ni.Theatrical && kind == "tv" && shape.Kind != "tv" {
		kind = "movie"
	}
	r.Kind, r.Confidence, r.Why = kind, conf, why
	if r.Title == "" {
		r.Title = strings.TrimSpace(name)
		r.Confidence = min(r.Confidence, 0.3)
	}
	return r
}
