package identify

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
)

// Several video files in one directory are versions of one film when they
// carry the same title (and, where both say so, the same year): the original
// and its AV1 re-encode, a cut and its hard-subbed copy. Jellyfin's
// VideoListResolver groups versions the same way, by name and year.
//
// recodeRe strips what only marks a derived copy: a re-encode prefix and a
// built-in-subtitle suffix.
var (
	recodePrefixRe = regexp.MustCompile(`(?i)^\W*av1[-_ .]*recode\W*`)
	subbedSuffixRe = regexp.MustCompile(`(?i)[-_. ]*sub_built_in$`)
	recodeNameRe   = regexp.MustCompile(`(?i)^\W*av1[-_ .]*recode`)
)

// workKey is the comparable identity of a video file: its title in lower case
// without punctuation, and its year (0 = none).
func workKey(name string) (title string, year int) {
	base := subbedSuffixRe.ReplaceAllString(stem(name), "")
	base = recodePrefixRe.ReplaceAllString(base, "")
	ni := ParseName(base+fileExt(name), true)
	if scraper.IsNoiseToken(ni.Title) {
		return "", 0
	}
	var b strings.Builder
	for _, r := range strings.ToLower(ni.Title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String(), ni.Year
}

func fileExt(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 && len(name)-i <= 6 {
		return name[i:]
	}
	return ""
}

// sameWork reports whether the videos are versions of one film. A file whose
// name says nothing but release words ("REMUX.mkv") joins the others.
func sameWork(videos []Entry) bool {
	title, year, keyed := "", 0, 0
	for _, v := range videos {
		t, y := workKey(v.Name)
		if t == "" {
			continue
		}
		if keyed > 0 {
			if t != title || (year > 0 && y > 0 && abs(year-y) > 1) {
				return false
			}
		}
		if keyed == 0 || year == 0 {
			title, year = t, y
		}
		keyed++
	}
	return keyed > 0
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// versionOrder lists the version files, the one to play by default first: the
// largest original, then the re-encodes.
func versionOrder(videos []Entry) []string {
	vs := append([]Entry(nil), videos...)
	sort.SliceStable(vs, func(i, j int) bool {
		ri, rj := recodeNameRe.MatchString(vs[i].Name), recodeNameRe.MatchString(vs[j].Name)
		if ri != rj {
			return !ri
		}
		if vs[i].Size != vs[j].Size {
			return vs[i].Size > vs[j].Size
		}
		return vs[i].Name < vs[j].Name
	})
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Name
	}
	return out
}

// videoYear is the year the largest of the videos names ("1917.2019.2160p.mkv"
// -> 2019), 0 when none does.
func videoYear(videos []Entry) int {
	best := -1
	for i, v := range videos {
		if best < 0 || v.Size > videos[best].Size {
			best = i
		}
	}
	if best < 0 {
		return 0
	}
	_, y := workKey(videos[best].Name)
	return y
}

var (
	yearInNameRe = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})(?:[^0-9]|$)`)
	latinWordRe  = regexp.MustCompile(`[A-Za-z]{5,}`)
)

// releasesOfOneFilm reports whether directories are copies of one film: they
// name the same year and share a title word ("…回忆三部曲[中文字幕].Memories.1995…"
// and "Memories _ Memorizu [Movie] [1995, BDRemux]"), and the first one holds
// a film. Needs a peek.
func releasesOfOneFilm(dirs []Entry, peek Peek) bool {
	if peek == nil || len(dirs) < 2 {
		return false
	}
	var common map[string]bool
	year := ""
	for _, d := range dirs {
		m := yearInNameRe.FindStringSubmatch(d.Name)
		if m == nil || (year != "" && m[1] != year) {
			return false
		}
		year = m[1]
		words := map[string]bool{}
		for _, w := range latinWordRe.FindAllString(d.Name, -1) {
			if lw := strings.ToLower(w); !scraper.IsNoiseToken(lw) {
				words[lw] = true
			}
		}
		if common == nil {
			common = words
			continue
		}
		for w := range common {
			if !words[w] {
				delete(common, w)
			}
		}
	}
	if len(common) == 0 {
		return false
	}
	kids, err := peek(dirs[0].Name)
	return err == nil && ClassifyEntries(kids).Kind == "movie"
}

// showDirBeside reports whether one of the directories next to a film holds a
// show (the Gantz series folder beside the Gantz:O film): different works.
func showDirBeside(dirs []Entry, peek Peek) bool {
	return peek != nil && slices.Contains(peekKinds(dirs, peek), "tv")
}

// unrelatedFilmDir reports whether a directory next to the seasons holds a
// film and does not carry the show's name (I Am Groot S01, S02 beside
// 银河护卫队3): another work. A film folder named after the show is its
// special (绝命D师电影 续命之徒), and a show kept under another name (Frieren)
// is still the show.
func unrelatedFilmDir(seasonNames []string, workDirs []Entry, peek Peek) bool {
	if peek == nil {
		return false
	}
	stems := make([]string, 0, len(seasonNames))
	for _, n := range seasonNames {
		if st := seasonStem(n); st != "" {
			stems = append(stems, st)
		}
	}
	if len(stems) == 0 {
		return false
	}
	var others []Entry
	for _, d := range workDirs {
		if seasonDirRe.MatchString(d.Name) || specialDirRe.MatchString(d.Name) {
			continue
		}
		o := seasonStem(d.Name)
		related := o == ""
		for _, st := range stems {
			if strings.HasPrefix(o, st) || strings.HasPrefix(st, o) {
				related = true
			}
		}
		if !related {
			others = append(others, d)
		}
	}
	for i, d := range others {
		if i >= maxPeek {
			break
		}
		if _, ok := episodeOf(d.Name); ok {
			continue // an episode kept in its own folder
		}
		kids, err := peek(d.Name)
		if err != nil || ClassifyEntries(kids).Kind != "movie" {
			continue
		}
		episode := false
		for _, k := range kids {
			if _, ok := episodeOf(k.Name); ok && IsVideo(k.Name) {
				episode = true
			}
		}
		if !episode {
			return true
		}
	}
	return false
}
