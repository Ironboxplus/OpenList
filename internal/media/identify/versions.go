package identify

import (
	"regexp"
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
