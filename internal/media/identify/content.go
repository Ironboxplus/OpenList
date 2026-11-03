package identify

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Entry is one directory entry as listed from storage.
type Entry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

var (
	videoExts = setOf("mkv", "mp4", "avi", "mov", "wmv", "flv", "ts", "m2ts", "rmvb", "rm", "webm", "mpg", "mpeg", "m4v", "vob", "iso", "mts")
	audioExts = setOf("mp3", "flac", "wav", "ape", "aac", "m4a", "ogg", "wma", "dsf", "dff", "opus", "alac")
)

func setOf(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func ext(name string) string { return strings.ToLower(strings.TrimPrefix(path.Ext(name), ".")) }

// IsVideo reports whether name has a video extension.
func IsVideo(name string) bool { return videoExts[ext(name)] }

func stem(name string) string {
	if e := path.Ext(name); e != "" && len(e) <= 6 {
		return strings.TrimSuffix(name, e)
	}
	return name
}

// Directory names that hold extras, not works.
var extraDirRe = regexp.MustCompile(`(?i)^(subs?|subtitles?|字幕|samples?([\s._-]*(videos?|clips?|files?))?|extras?|featurettes?|trailers?|pictures?|photos?|posters?|behind[\s._-]*the[\s._-]*scenes|scans?|cds?\d*|bdmv|certificate|menu|screens?|screenshots?|covers?|artworks?|nced|ncop|sps?|bonus|special features?|cd\d+|disc\s*\d+|dvd\d*|\.?[a-z_]*(thumb|cache)[a-z_]*)$`)

// bracketsRe strips the brackets some releases put around an extras directory
// name ("[Subs]", "【字幕】").
var bracketsRe = regexp.MustCompile(`[\[\]()（）【】]`)

// orderPrefixRe is the "1. " some releases put before directory names to sort them.
var orderPrefixRe = regexp.MustCompile(`^\s*\d{1,2}\s*[.\-_)]\s*`)

func isExtraDirName(name string) bool {
	n := strings.TrimSpace(bracketsRe.ReplaceAllString(name, ""))
	return extraDirRe.MatchString(n) || extraDirRe.MatchString(orderPrefixRe.ReplaceAllString(n, "")) || ostRe.MatchString(name)
}

// junkVideoRe names files that are never the work: samples, trailers, teasers.
var junkVideoRe = regexp.MustCompile(`(?i)(^|[\s._\[(-])(sample|trailer|teaser|featurette)s?([\s._\])-]|$)`)

// mainVideos drops what is not the work from a list of videos: files named
// like samples or trailers, and files far smaller than the largest one (a few
// megabytes next to gigabytes). A set where nothing passes is returned as it was.
func mainVideos(videos []Entry) []Entry {
	var largest int64
	for _, v := range videos {
		largest = max(largest, v.Size)
	}
	out := make([]Entry, 0, len(videos))
	for _, v := range videos {
		if junkVideoRe.MatchString(stem(v.Name)) {
			continue
		}
		if v.Size > 0 && v.Size*20 < largest && v.Size < 300<<20 {
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return videos
	}
	return out
}

// Directory names that are seasons of one show: the keyword forms at the
// start ("Season 2", "第3季", "Specials"), and a season marker anywhere in the
// name, because release folders carry the show name first ("Friends.S01.1080p",
// "葬送的芙莉莲 S01", "BEASTARS 2nd Season") or are only a number ("1", "02").
// Roman numerals are not numbering (无间道 I/II/III are different films).
var seasonDirRe = regexp.MustCompile(`(?i)^(season|series|s)[\s._-]*\d{1,2}\b|^第\s*[\d一二三四五六七八九十百]+\s*[季部期]|^specials?$|^ova$|^oad$|^特典|^特别篇|^剧场版$|(?:^|[^a-z0-9])s\d{1,3}(?:[^a-z0-9]|$)|第\s*[\d一二三四五六七八九十百]+\s*[季部期]|(?:^|[^a-z0-9])season[\s._-]*\d{1,2}(?:[^0-9]|$)|\d{1,2}(?:st|nd|rd|th)[\s._-]*season|^0?\d{1,2}$|^s\d{1,2}(?:\D|$)|^se\d{2}(?:\D|$)`)

type epMatch struct {
	Season, Episode int
	Cut             int // index in the stem where the marker starts
}

var episodePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)S(\d{1,2})\s*E(\d{1,3})`),
	regexp.MustCompile(`(?i)(?:^|[\s._\[(-])E(?:P)?\s*(\d{1,3})(?:[\s._\])-]|$)`),
	regexp.MustCompile(`第\s*(\d{1,3})\s*[集话話]`),
	regexp.MustCompile(`\[(\d{1,3})(?:v\d)?\]`),
	regexp.MustCompile(`【\s*(\d{1,3})\s*[.．。]`),
	regexp.MustCompile(` - (\d{1,3})(?:v\d)?(?: |$|\[)`),
}

// episodeOf finds an episode marker in a file name.
func episodeOf(name string) (epMatch, bool) {
	s := stem(name)
	for i, re := range episodePatterns {
		m := re.FindStringSubmatchIndex(s)
		if m == nil {
			continue
		}
		get := func(k int) int {
			n, _ := strconv.Atoi(s[m[2*k]:m[2*k+1]])
			return n
		}
		if i == 0 {
			return epMatch{Season: get(1), Episode: get(2), Cut: m[0]}, true
		}
		return epMatch{Episode: get(1), Cut: m[0]}, true
	}
	return epMatch{}, false
}

func isDigitRune(r rune) bool {
	return unicode.IsDigit(r) || strings.ContainsRune("一二三四五六七八九十〇零", r)
}

// numberedSeries applies the lexicon-free rule: siblings that share a prefix
// and differ only by a short number (an episode or part number) are one
// series; siblings whose first difference is a letter or a character are
// different works. Years (4 digits) do not count as numbering, and neither do
// roman numerals.
func numberedSeries(names []string) bool {
	if len(names) < 2 {
		return false
	}
	rs := make([][]rune, len(names))
	for i, n := range names {
		rs[i] = []rune(strings.ToLower(n))
	}
	lcp := len(rs[0])
	for _, r := range rs[1:] {
		l := 0
		for l < lcp && l < len(r) && r[l] == rs[0][l] {
			l++
		}
		lcp = l
	}
	for _, r := range rs {
		if lcp >= len(r) {
			continue // this name is the bare prefix: part of the series
		}
		if !isDigitRune(r[lcp]) {
			return false
		}
		n := 0
		for k := lcp; k < len(r) && unicode.IsDigit(r[k]); k++ {
			n++
		}
		if n > 3 {
			return false
		}
	}
	return true
}

// Shape is what a directory's content says it is.
type Shape struct {
	Kind       string // movie | tv | collection | ignore | "" (can't tell)
	Confidence float32
	Why        string
	Episodes   int      // video files that look like episodes
	Versions   []string // movie: the files that are versions of it, default first
	Year       int      // movie: the year its main video file names, 0 = none
}

// Peek lists the subdirectory name of the directory being classified. It is
// optional: the rules that need to look one level further down ask it, and
// give the same answer as before without it.
type Peek func(name string) ([]Entry, error)

// maxPeek is how many subdirectories a verdict looks into.
const maxPeek = 3

// peekKinds is the shape of up to maxPeek of the subdirectories, "" for one
// that cannot be listed; nil when there is no peek.
func peekKinds(dirs []Entry, peek Peek) []string {
	if peek == nil {
		return nil
	}
	var out []string
	for i, d := range dirs {
		if i >= maxPeek {
			break
		}
		kids, err := peek(d.Name)
		if err != nil {
			out = append(out, "")
			continue
		}
		out = append(out, ClassifyEntries(kids).Kind)
	}
	return out
}

// specialDirRe are directory names that are extras of a show or a film rather
// than seasons when nothing else says "show".
var specialDirRe = regexp.MustCompile(`(?i)^specials?$|^ova$|^oad$|^特典|^特别篇|^剧场版$`)

// ClassifyEntries decides a directory's role from one listing. It never needs
// a second level: a "Season x" directory or a run of numbered episodes is
// enough for tv, several directories that are not a numbered series are a
// collection.
func ClassifyEntries(entries []Entry) Shape { return ClassifyEntriesWith(entries, nil) }

// ClassifyEntriesWith is ClassifyEntries that may look into a few
// subdirectories where one listing cannot tell: numbered directories that are
// films ("死神来了1", "死神来了2") or seasons, and a directory that only wraps
// another one.
func ClassifyEntriesWith(entries []Entry, peek Peek) Shape {
	var videos, dirs []Entry
	var audio, other int
	for _, e := range entries {
		switch {
		case e.IsDir:
			dirs = append(dirs, e)
		case IsVideo(e.Name):
			videos = append(videos, e)
		case audioExts[ext(e.Name)]:
			audio++
		default:
			other++
		}
	}
	videos = mainVideos(videos)
	if len(entries) == 0 {
		return Shape{Why: "empty listing"}
	}
	if len(videos) == 0 && len(dirs) == 0 {
		return Shape{Kind: "ignore", Confidence: 0.85, Why: "no video and no subdirectory"}
	}

	var workDirs, seasonDirs []Entry
	var seasonNames []string
	var specials []Entry
	for _, d := range dirs {
		switch {
		case specialDirRe.MatchString(d.Name):
			specials = append(specials, d)
		case seasonDirRe.MatchString(d.Name):
			seasonNames = append(seasonNames, d.Name)
			seasonDirs = append(seasonDirs, d)
			workDirs = append(workDirs, d)
		case isExtraDirName(d.Name):
		default:
			workDirs = append(workDirs, d)
		}
	}
	// "特典" / "Specials" next to a lone film are its extras; next to
	// seasons, or alone, they are what they say.
	if len(seasonNames) > 0 || len(videos) == 0 {
		for _, d := range specials {
			seasonNames = append(seasonNames, d.Name)
			seasonDirs = append(seasonDirs, d)
			workDirs = append(workDirs, d)
		}
	}
	seasons := len(seasonNames)
	if !oneShow(seasonNames) {
		seasons = 0 // seasons of different shows: each directory is its own work
	}
	// A single season next to a directory whose name goes on from the show's
	// ("FLCL S01", "FLCL Alternative & Progressive") is a sequel, a work of
	// its own: a collection.
	if seasons == 1 && extendsSeason(seasonNames[0], workDirs) {
		seasons = 0
	}
	if seasons >= 1 && unrelatedFilmDir(seasonNames, workDirs, peek) {
		return Shape{Kind: "collection", Confidence: 0.75, Why: "seasons of a show next to an unrelated film"}
	}
	// Bare numbers ("1", "2") are seasons only when what they hold is a show:
	// 与神同行's "1" and "2" are two films.
	if seasons > 0 && allBareNumbers(seasonNames) {
		if filmsNotShows(peekKinds(seasonDirs, peek)) {
			return Shape{Kind: "collection", Confidence: 0.75, Why: "numbered directories that each hold one film"}
		}
	}

	epCount := 0
	var epStems []string
	for _, v := range videos {
		if m, ok := episodeOf(v.Name); ok {
			epCount++
			epStems = append(epStems, stem(v.Name)[:m.Cut])
		}
	}
	episodic := len(videos) >= 2 && epCount >= 2 && epCount*10 >= len(videos)*6

	switch {
	case seasons >= 1 && (seasons >= 2 || len(videos) == 0 || episodic):
		return Shape{Kind: "tv", Confidence: 0.9, Why: "season directories", Episodes: epCount}
	case seasons == 1:
		return Shape{Kind: "tv", Confidence: 0.75, Why: "one season directory", Episodes: epCount}
	case episodic:
		return Shape{Kind: "tv", Confidence: 0.9, Why: "numbered episode files", Episodes: epCount}
	}
	if len(videos) >= 3 {
		names := make([]string, len(videos))
		for i, v := range videos {
			names[i] = stem(v.Name)
		}
		if numberedSeries(names) {
			return Shape{Kind: "tv", Confidence: 0.8, Why: "files differ only by a number", Episodes: len(videos)}
		}
	}
	if len(videos) == 0 {
		names := make([]string, len(workDirs))
		for i, d := range workDirs {
			names[i] = d.Name
		}
		switch {
		case len(workDirs) >= 2 && numberedSeries(names):
			// "死神来了1".."死神来了5" are five films, "蓝光原盘"-style parts one show
			if filmsNotShows(peekKinds(workDirs, peek)) {
				return Shape{Kind: "collection", Confidence: 0.75, Why: "numbered directories that each hold one film"}
			}
			return Shape{Kind: "tv", Confidence: 0.7, Why: "numbered subdirectories (parts of one show)"}
		case releasesOfOneFilm(workDirs, peek):
			return Shape{Kind: "movie", Confidence: 0.7, Why: "directories that are copies of one film"}
		case len(workDirs) >= 2:
			return Shape{Kind: "collection", Confidence: 0.8, Why: "several subdirectories, each its own work"}
		case len(workDirs) == 1:
			// a wrapper: what it holds is what this is
			if k := peekKinds(workDirs, peek); len(k) == 1 && (k[0] == "movie" || k[0] == "tv") {
				return Shape{Kind: k[0], Confidence: 0.7, Why: "the only subdirectory holds " + k[0]}
			}
			return Shape{Why: "single wrapper subdirectory"}
		}
		return Shape{Why: "only extra directories"}
	}
	// videos present, not episodic
	if len(videos) >= 1 && showDirBeside(workDirs, peek) {
		return Shape{Kind: "collection", Confidence: 0.75, Why: "a show beside a film"}
	}
	if len(workDirs) >= 2 && len(videos) <= 1 {
		return Shape{Kind: "collection", Confidence: 0.75, Why: "several subdirectories next to at most one video"}
	}
	if len(videos) >= 2 && sameWork(videos) {
		return Shape{Kind: "movie", Confidence: 0.85, Why: "versions of one movie", Versions: versionOrder(videos), Year: videoYear(videos)}
	}
	if len(videos) == 1 || dominant(videos) {
		return Shape{Kind: "movie", Confidence: 0.85, Why: "one main video", Year: videoYear(videos)}
	}
	if multiPart(videos) {
		return Shape{Kind: "movie", Confidence: 0.8, Why: "parts of one movie"}
	}
	return Shape{Kind: "collection", Confidence: 0.7, Why: "several unrelated videos"}
}

var bareNumberRe = regexp.MustCompile(`^\s*0*\d{1,2}\s*$`)

func allBareNumbers(names []string) bool {
	for _, n := range names {
		if !bareNumberRe.MatchString(n) {
			return false
		}
	}
	return len(names) > 0
}

// extendsSeason reports whether another work directory's name is the season's
// show name followed by more words ("flcl" -> "flclalternativeprogressive").
func extendsSeason(season string, workDirs []Entry) bool {
	st := seasonStem(season)
	if st == "" {
		return false
	}
	for _, d := range workDirs {
		if d.Name == season || seasonDirRe.MatchString(d.Name) {
			continue
		}
		o := seasonStem(d.Name)
		if len(o) > len(st) && strings.HasPrefix(o, st) {
			return true
		}
	}
	return false
}

// dominant: one video is at least 80% of all video bytes (the rest are
// trailers and samples).
func dominant(videos []Entry) bool {
	var total, max int64
	for _, v := range videos {
		total += v.Size
		if v.Size > max {
			max = v.Size
		}
	}
	return total > 0 && max*10 >= total*8
}

var partRe = regexp.MustCompile(`(?i)[\s._\[(-](cd|disc|disk|part|pt)\s*\d`)

func multiPart(videos []Entry) bool {
	n := 0
	for _, v := range videos {
		if partRe.MatchString(stem(v.Name)) {
			n++
		}
	}
	return n == len(videos)
}

// EpisodeOf finds the season and episode a file name carries. Season is 0
// when the name has an episode number but no season ("S01E03" gives 1).
func EpisodeOf(name string) (season, episode int, ok bool) {
	m, ok := episodeOf(name)
	return m.Season, m.Episode, ok
}

// IsSeasonDir reports whether a directory name is a season of a show.
func IsSeasonDir(name string) bool { return seasonDirRe.MatchString(name) }

// IsExtraDir reports whether a directory name holds extras, not a work.
func IsExtraDir(name string) bool { return isExtraDirName(name) }

// SeasonOfDir returns the season number a directory name carries ("Season 2",
// "第3季"), or 0.
func SeasonOfDir(name string) int {
	if m := seasonRe.FindStringSubmatch(name); m != nil {
		switch {
		case m[1] != "":
			n, _ := strconv.Atoi(m[1])
			return n
		case m[3] != "":
			n, _ := strconv.Atoi(m[3])
			return n
		case m[4] != "":
			return cnToInt(m[4])
		}
	}
	return 0
}

var (
	ordinalSeasonRe = regexp.MustCompile(`(?i)(\d{1,2})(?:st|nd|rd|th)[\s._-]*season`)
	bareSeasonRe    = regexp.MustCompile(`^0*(\d{1,2})$`)
	bracketGroupRe  = regexp.MustCompile(`[\[【(（][^\]】)）]*[\]】)）]`)
)

// seasonOfDirName is SeasonOfDir plus the forms only a directory next to its
// sibling seasons can be trusted with: "2nd Season" and a bare "02".
func seasonOfDirName(name string) int {
	if n := SeasonOfDir(name); n > 0 {
		return n
	}
	if m := ordinalSeasonRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if m := bareSeasonRe.FindStringSubmatch(strings.TrimSpace(name)); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// seasonStem is the show name a season directory starts with: the text before
// its season marker, without release-group brackets, case, punctuation and
// "&"/"and" differences. Empty when the name is only a marker ("Season 1", "1").
func seasonStem(name string) string {
	s := bracketGroupRe.ReplaceAllString(name, " ")
	if loc := seasonDirRe.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	s = strings.ReplaceAll(strings.ToLower(s), "&", " and ")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// oneShow reports whether season directories belong to one show: their stems
// agree (equal, one a prefix of the other, or empty). Season directories of
// different shows ("Daredevil S01", "Jessica Jones S01") are a collection.
func oneShow(names []string) bool {
	ref := ""
	for _, n := range names {
		st := seasonStem(n)
		switch {
		case st == "":
		case ref == "":
			ref = st
		case !strings.HasPrefix(st, ref) && !strings.HasPrefix(ref, st):
			return false
		}
	}
	return true
}

// filmsNotShows: among the peeked kinds there is a film and no show (a
// directory that only wraps another one cannot say, and is not held against it).
func filmsNotShows(kinds []string) bool {
	films := false
	for _, k := range kinds {
		switch k {
		case "tv":
			return false
		case "movie":
			films = true
		}
	}
	return films
}
