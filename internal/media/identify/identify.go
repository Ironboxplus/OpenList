// Package identify decides what a directory (or a loose video) in a library
// is: its kind, title, year and season. The name gives the title; the
// content, listed once, gives the kind. Names alone cannot tell a movie from
// a show ("行尸走肉" and "寄生虫" look the same), which is why the kind comes
// from what is inside.
package identify

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
)

// Result is what the rules concluded about one item.
type Result struct {
	Kind        string // movie | tv | anime | collection | ignore
	Theatrical  bool   // an anime movie (剧场版): look it up as a movie
	Title       string
	English     string
	Year        int
	Season      int
	Episodes    int     // announced episode count, 0 = unknown
	Confidence  float32 // how sure the rules are about Kind
	Why         string
	Parsed      scraper.ParsedName // for building search terms
	Group       string
	TMDBID      string // inline id tag in the name (digits); wins over any search
	IMDbID      string
	Versions    []string      // movie: the files that are versions of it, default first
	FromContent bool          // Kind comes from what the directory holds, not from its name
	Content     []ContentName // English names found inside the directory, for the provider search
	Firm        bool          // collection by positive evidence; see Shape.Firm
	ChildKinds  []string      // what the first subdirectories hold, when a rule looked
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
	return ClassifyWith(name, isDir, entries, nil)
}

// ClassifyWith is Classify that may look into a few subdirectories (see
// ClassifyEntriesWith).
func ClassifyWith(name string, isDir bool, entries []Entry, peek Peek) Result {
	if isDir {
		entries = UnwrapEntries(entries, peek)
	}
	// A directory named like its video and holding only that video is the file
	// (115 offline downloads): it is read by its name without the extension.
	wrapper := isDir && IsVideo(name) && entries != nil && holdsOneVideo(entries)
	ni := ParseName(name, !isDir || wrapper)
	r := Result{
		Title: ni.Title, English: ni.English, Year: ni.Year, Season: ni.Season,
		Episodes: ni.Episodes, Parsed: ni.Parsed, Group: ni.Group, TMDBID: ni.TMDBID, IMDbID: ni.IMDbID,
	}

	var shape Shape
	switch {
	case !isDir || wrapper:
		if ni.TVHint && !ni.Collection {
			shape = Shape{Kind: "tv", Confidence: 0.6, Why: "episode marker in a file name"}
		} else {
			shape = Shape{Kind: "movie", Confidence: 0.85, Why: "a single video file"}
		}
	case entries != nil:
		shape = ClassifyEntriesWith(entries, peek)
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

	// A music release (an album set, a live recording): its name tags an audio
	// format and no video one, and no video file is in it.
	if kind != "ignore" && audioTagRe.MatchString(name) && !videoTagRe.MatchString(name) && !hasVideoFile(entries) {
		kind, conf, why = "ignore", 0.7, "audio format tag in the name and no video"
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
	r.Content = shape.Content
	r.Firm, r.ChildKinds = shape.Firm, shape.ChildKinds
	r.FromContent = shape.Kind != "" && shape.Kind == kind
	if kind == "movie" || kind == "anime" {
		r.Versions = shape.Versions
	}
	// A directory name without a year takes the year its video file names:
	// "1917" holding 1917.2019.2160p.mkv is the film of 2019.
	if r.Year == 0 && shape.Year > 0 {
		r.Year = shape.Year
		r.Parsed.Year = strconv.Itoa(shape.Year)
	}
	if r.Title == "" {
		r.Title = strings.TrimSpace(name)
		r.Confidence = min(r.Confidence, 0.3)
	}
	return r
}

// PeekClassifier is a Classifier that can look into a few subdirectories.
type PeekClassifier interface {
	ClassifyWith(ctx context.Context, name string, isDir bool, entries []Entry, peek Peek) (Result, error)
}

// ClassifyWith implements PeekClassifier.
func (Rules) ClassifyWith(_ context.Context, name string, isDir bool, entries []Entry, peek Peek) (Result, error) {
	return ClassifyWith(name, isDir, entries, peek), nil
}

// MaybeShow reports whether a collection verdict may be one show whose
// subdirectories are its seasons or releases (魔道祖师, 星蝶公主): the rules took
// it for several works only because nothing said otherwise, and no
// subdirectory was seen to hold a film. Only a provider can say more.
func (r Result) MaybeShow() bool {
	return r.Kind == "collection" && r.FromContent && !r.Firm && len(r.ChildKinds) > 0 && !slices.Contains(r.ChildKinds, "movie")
}

// bareName reports whether a file or directory name has no title of its own:
// a number of one or two digits ("1", "02", the second film of 与神同行), or
// nothing but release and language tags (纯英, 中英, 1080p). num is the number
// without leading zeros, "" for tags.
func bareName(name string, isFile bool) (num string, ok bool) {
	st := name
	if isFile {
		st = stem(name)
	}
	st = strings.TrimSpace(st)
	switch {
	case st == "":
		return "", false
	case isAllDigits(st):
		if len(st) > 2 {
			return "", false // "300", "1917" are titles
		}
		n, _ := strconv.Atoi(st)
		return strconv.Itoa(n), true
	case isTechBracket(st):
		return "", true
	}
	return "", false
}

// HasNoTitle reports whether a file or directory name carries no title of its
// own: a number of one or two digits, or nothing but release and language tags.
func HasNoTitle(name string, isFile bool) bool {
	_, ok := bareName(name, isFile)
	return ok
}

// InheritParentTitle gives a member of a collection that has no title of its
// own (a directory "2", a release tag) the title of the collection and its
// number, so it is looked up as "与神同行 2" and not as "2". The year and the
// names found inside the member stay its own: they say which work of the
// collection it is. It reports whether the member took the title.
func (r *Result) InheritParentTitle(name string, isFile bool, parentName string) bool {
	if r.Kind == "collection" || r.Kind == "ignore" {
		return false
	}
	num, ok := bareName(name, isFile)
	if !ok {
		return false
	}
	pi := ParseName(parentName, false)
	if pi.Title == "" {
		return false
	}
	if _, bare := bareName(parentName, false); bare {
		return false
	}
	title := pi.Title
	parsed := pi.Parsed
	if num != "" {
		title += " " + num
		switch {
		case parsed.Chinese != "":
			parsed.Chinese += " " + num
		case parsed.English != "":
			parsed.English += " " + num
		}
	}
	if r.Year > 0 {
		parsed.Year = strconv.Itoa(r.Year)
	}
	r.Title, r.Parsed, r.English = title, parsed, pi.English
	return true
}
