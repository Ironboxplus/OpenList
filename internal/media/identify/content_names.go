package identify

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
)

// ContentName is a title read from inside a directory: the English name of its
// video file or of its season folders, with the year they name.
type ContentName = scraper.ContentName

// maxContentNames is how many names a directory hands to the search.
const maxContentNames = 3

var (
	// seasonTailRe cuts a name at its season or episode marker.
	seasonTailRe = regexp.MustCompile(`(?i)[\s._\-\[(]+(?:S\d{1,3}(?:E\d{1,3})?|Season[\s._-]*\d+|\d{1,2}(?:st|nd|rd|th)[\s._-]*season)(?:\b|$).*$|\s*第\s*[\d一二三四五六七八九十]+\s*[季部期].*$`)
	// titleYearRe finds "Title.1995." in a name whose parsed title came out Chinese.
	titleYearRe = regexp.MustCompile(`([A-Za-z][A-Za-z'’&: ]*[A-Za-z])[ ._]+((?:19|20)\d{2})(?:[^0-9]|$)`)
)

// contentName reads the Latin title and the year out of one file or directory
// name ("One.Flew.Over.the.Cuckoos.Nest.1975.REMUX.mkv", "Love.Death.&.Robots.S01"),
// cut at the season marker. ok is false when the name holds no usable title
// (a language tag, a release word, a single short word).
func contentName(name string, isFile bool) (ContentName, bool) {
	ni := ParseName(name, isFile)
	en, year := ni.English, ni.Year
	raw := name
	if isFile {
		raw = stem(name)
	}
	if year == 0 {
		if m := yearInNameRe.FindStringSubmatch(raw); m != nil {
			year, _ = strconv.Atoi(m[1])
		}
	}
	if en == "" || hasHan(en) {
		en = ""
		if m := titleYearRe.FindStringSubmatch(strings.NewReplacer("_", " ").Replace(raw)); m != nil {
			en = m[1]
			year, _ = strconv.Atoi(m[2])
		}
	}
	en = strings.TrimSpace(seasonTailRe.ReplaceAllString(en, ""))
	if en == "" || scraper.IsNoiseToken(en) || IsExtraDir(en) {
		return ContentName{}, false
	}
	letters, words := 0, len(strings.Fields(en))
	for _, r := range en {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < 4 || (words == 1 && letters < 5) || hasHan(en) {
		return ContentName{}, false
	}
	return ContentName{Name: en, Year: year}, true
}

// contentNames lists the titles a directory's content gives, best first: the
// name of its main video, then the names of its subdirectories. A directory
// that only wraps another one gives that one's names (needs peek).
func contentNames(entries []Entry, peek Peek) []ContentName {
	var out []ContentName
	seen := map[string]bool{}
	add := func(c ContentName, ok bool) {
		key := strings.ToLower(c.Name)
		if ok && !seen[key] && len(out) < maxContentNames {
			seen[key] = true
			out = append(out, c)
		}
	}
	var videos, dirs []Entry
	for _, e := range entries {
		switch {
		case e.IsDir:
			if !isExtraDirName(e.Name) {
				dirs = append(dirs, e)
			}
		case IsVideo(e.Name):
			videos = append(videos, e)
		}
	}
	if videos = mainVideos(videos); len(videos) > 0 {
		best := videos[0]
		for _, v := range videos[1:] {
			if v.Size > best.Size {
				best = v
			}
		}
		add(contentName(best.Name, true))
	}
	slices.SortFunc(dirs, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	for _, d := range dirs {
		add(contentName(d.Name, false))
	}
	if len(out) == 0 && len(videos) == 0 && len(dirs) == 1 && peek != nil {
		if kids, err := peek(dirs[0].Name); err == nil {
			return contentNames(kids, nil)
		}
	}
	return out
}
