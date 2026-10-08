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
var extraDirRe = regexp.MustCompile(`(?i)^(subs?|subtitles?|字幕|sample|samples|extras?|featurettes?|scans?|cds?\d*|bdmv|certificate|menu|screens?|screenshots?|covers?|artworks?|nced|ncop|sps?|bonus|special features?|cd\d+|disc\s*\d+|dvd\d*|\.?[a-z_]*(thumb|cache)[a-z_]*)$`)

// Directory names that are seasons of one show.
var seasonDirRe = regexp.MustCompile(`(?i)^(season|series|s)[\s._-]*\d{1,2}\b|^第\s*[\d一二三四五六七八九十百]+\s*[季部期]|^specials?$|^ova$|^oad$|^特典|^特别篇|^剧场版$`)

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
	Episodes   int // video files that look like episodes
}

// ClassifyEntries decides a directory's role from one listing. It never needs
// a second level: a "Season x" directory or a run of numbered episodes is
// enough for tv, several directories that are not a numbered series are a
// collection.
func ClassifyEntries(entries []Entry) Shape {
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
	if len(entries) == 0 {
		return Shape{Why: "empty listing"}
	}
	if len(videos) == 0 && len(dirs) == 0 {
		return Shape{Kind: "ignore", Confidence: 0.85, Why: "no video and no subdirectory"}
	}

	var workDirs []Entry
	seasons := 0
	for _, d := range dirs {
		switch {
		case seasonDirRe.MatchString(d.Name):
			seasons++
			workDirs = append(workDirs, d)
		case extraDirRe.MatchString(d.Name):
		default:
			workDirs = append(workDirs, d)
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
			return Shape{Kind: "tv", Confidence: 0.7, Why: "numbered subdirectories (parts of one show)"}
		case len(workDirs) >= 2:
			return Shape{Kind: "collection", Confidence: 0.8, Why: "several subdirectories, each its own work"}
		case len(workDirs) == 1:
			return Shape{Why: "single wrapper subdirectory"}
		}
		return Shape{Why: "only extra directories"}
	}
	// videos present, not episodic
	if len(workDirs) >= 2 && len(videos) <= 1 {
		return Shape{Kind: "collection", Confidence: 0.75, Why: "several subdirectories next to at most one video"}
	}
	if len(videos) == 1 || dominant(videos) {
		return Shape{Kind: "movie", Confidence: 0.85, Why: "one main video"}
	}
	if multiPart(videos) {
		return Shape{Kind: "movie", Confidence: 0.8, Why: "parts of one movie"}
	}
	return Shape{Kind: "collection", Confidence: 0.7, Why: "several unrelated videos"}
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
func IsExtraDir(name string) bool { return extraDirRe.MatchString(name) }

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
