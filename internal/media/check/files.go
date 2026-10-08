package check

import (
	"context"
	"errors"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/media/apply"
	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
)

// MaxDepth is how many directory levels below a work are listed.
const MaxDepth = 3

// file is one file under a work. Rel is its slash path below the work.
type file struct {
	Rel  string
	Dir  string // Rel's directory, "" for the top
	Name string
	Size int64
}

var (
	featureExts = map[string]bool{"mkv": true, "mp4": true, "avi": true, "ts": true, "m2ts": true, "rmvb": true, "iso": true}
	subExts     = map[string]bool{"ass": true, "ssa": true, "srt": true, "sup": true}

	// Directories that hold extras, not the feature: SPs, menu, 特典*,
	// Extras, CDs, Scans, anything with PV / CM in it (plus Specials and
	// Samples).
	excludedDirRe = regexp.MustCompile(`(?i)^(sps?|menu|extras?|cds?\d*|scans?|specials?|samples?)$|^特典|(?:^|[^a-z0-9])(pv|cm)\d*(?:[^a-z]|$)`)
	// File names of extras: NCOP, NCED, Menu, PV, CM, Preview, SP01.
	excludedFileRe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(ncop|nced|menu|pv|cm|preview|sp\d+)\d*(?:[^a-z]|$)`)

	hintRe = regexp.MustCompile(`内封|内嵌|简繁|简日|繁日|字幕|(?i:sub\(chs)|(?i:(?:^|[^a-z])(?:chs|cht)(?:[^a-z]|$))`)

	skipDescendRe = excludedDirRe
	subDirRe      = regexp.MustCompile(`(?i)^(subs?|subtitles?|字幕)$`)
)

func extOf(name string) string { return strings.ToLower(strings.TrimPrefix(path.Ext(name), ".")) }

// segments of a slash path.
func segments(rel string) []string {
	if rel == "" {
		return nil
	}
	return strings.Split(rel, "/")
}

// isFeature applies the defaults, or the filter of a rule: the filter decides
// instead of the exclusion lists, but the file still has to be a video.
func isFeature(c *compiled, f file) bool {
	if c != nil && c.filter != nil {
		return identify.IsVideo(f.Name) && c.filter.IncludeRemote(f.Rel)
	}
	if !featureExts[extOf(f.Name)] {
		return false
	}
	for _, s := range segments(f.Dir) {
		if excludedDirRe.MatchString(s) {
			return false
		}
	}
	return !excludedFileRe.MatchString(f.Name)
}

type listing struct {
	Features []file
	Subs     []file
	Calls    int
	Errs     []string
	Single   bool // the work is one file, found through its parent
}

// collect lists a work serially, at most MaxDepth levels deep. A work that is
// a single file (a movie at the top of the library) is found through its
// parent, together with the subtitles that share its name.
func collect(ctx context.Context, l apply.Lister, root string, c *compiled) listing {
	var out listing
	type dirJob struct {
		abs, rel string
		depth    int
	}
	queue := []dirJob{{root, "", 1}}
	for len(queue) > 0 {
		j := queue[0]
		queue = queue[1:]
		if ctx.Err() != nil {
			out.Errs = append(out.Errs, ctx.Err().Error())
			return out
		}
		entries, err := l.List(ctx, j.abs)
		out.Calls++
		if err != nil {
			if errors.Is(err, apply.ErrNotDir) && j.depth == 1 {
				return collectSingle(ctx, l, root, c, out.Calls)
			}
			out.Errs = append(out.Errs, j.abs+": "+err.Error())
			continue
		}
		sort.Slice(entries, func(a, b int) bool { return entries[a].Name < entries[b].Name })
		for _, e := range entries {
			rel := path.Join(j.rel, e.Name)
			if e.IsDir {
				if j.depth >= MaxDepth {
					continue
				}
				if (c == nil || c.filter == nil) && skipDescendRe.MatchString(e.Name) && !subDirRe.MatchString(e.Name) {
					continue
				}
				queue = append(queue, dirJob{j.abs + "/" + e.Name, rel, j.depth + 1})
				continue
			}
			out.add(file{Rel: rel, Dir: j.rel, Name: e.Name, Size: e.Size}, c)
		}
	}
	return out
}

func (o *listing) add(f file, c *compiled) {
	switch {
	case subExts[extOf(f.Name)]:
		o.Subs = append(o.Subs, f)
	case isFeature(c, f):
		o.Features = append(o.Features, f)
	}
}

func collectSingle(ctx context.Context, l apply.Lister, p string, c *compiled, calls int) listing {
	out := listing{Calls: calls, Single: true}
	parent, name := path.Dir(p), path.Base(p)
	entries, err := l.List(ctx, parent)
	out.Calls++
	if err != nil {
		out.Errs = append(out.Errs, parent+": "+err.Error())
		return out
	}
	st := strings.ToLower(strings.TrimSuffix(name, path.Ext(name)))
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		f := file{Rel: e.Name, Name: e.Name, Size: e.Size}
		switch {
		case e.Name == name:
			out.add(f, c)
		case subExts[extOf(e.Name)] && strings.HasPrefix(strings.ToLower(e.Name), st):
			out.add(f, c)
		}
	}
	return out
}

// episodeOf finds the episode a feature file is: the rule's pattern if there
// is one, else the shared rules (SxxEyy, [01], - 01, 第01集). season is 0 when
// the name carries none.
func episodeOf(c *compiled, name string) (season, ep int, ok bool) {
	if c != nil && c.epRe != nil {
		m := c.epRe.FindStringSubmatch(name)
		if m == nil || m[c.epIdx] == "" {
			return 0, 0, false
		}
		ep = atoi(m[c.epIdx])
		if c.seasonIx > 0 && m[c.seasonIx] != "" {
			season = atoi(m[c.seasonIx])
		}
		return season, ep, ep > 0
	}
	s, e, ok := identify.EpisodeOf(name)
	return s, e, ok && e > 0
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// seasonOfDir is the season a file's directories say ("Season 2", "第3季"),
// nearest directory first; 0 when none does.
func seasonOfDir(dir string) int {
	segs := segments(dir)
	for i := len(segs) - 1; i >= 0; i-- {
		if n := identify.SeasonOfDir(segs[i]); n > 0 {
			return n
		}
	}
	return 0
}

// --- subtitles ---

var langRes = []struct {
	lang string
	re   *regexp.Regexp
}{
	{"chs", regexp.MustCompile(`(?i)(?:^|[^a-z])(chs|sc|gb)(?:[^a-z]|$)|简`)},
	{"cht", regexp.MustCompile(`(?i)(?:^|[^a-z])(cht|tc|big5)(?:[^a-z]|$)|繁`)},
	{"jpn", regexp.MustCompile(`(?i)(?:^|[^a-z])(jpn|jp|ja)(?:[^a-z]|$)|日`)},
	{"eng", regexp.MustCompile(`(?i)(?:^|[^a-z])(eng|en)(?:[^a-z]|$)|英`)},
}

// subLangs reads the languages from what follows the video's name in the
// subtitle's name (Show - 01.chs.ass: ".chs"), or from the whole name when it
// doesn't share the video's prefix. No hit gives "und".
func subLangs(subName, featStem string) []string {
	s := strings.TrimSuffix(subName, path.Ext(subName))
	if featStem != "" && strings.HasPrefix(strings.ToLower(s), strings.ToLower(featStem)) {
		s = s[len(featStem):]
	}
	var out []string
	for _, l := range langRes {
		if l.re.MatchString(s) {
			out = append(out, l.lang)
		}
	}
	if len(out) == 0 {
		return []string{"und"}
	}
	return out
}

// trackLang maps a Matroska language code to the labels subLangs uses. Chinese
// can't be told apart by the code alone, so it counts for both.
func trackLang(code string) []string {
	code = strings.ToLower(code)
	switch {
	case code == "jpn" || code == "ja" || strings.HasPrefix(code, "ja-"):
		return []string{"jpn"}
	case code == "eng" || code == "en" || strings.HasPrefix(code, "en-"):
		return []string{"eng"}
	case code == "chi" || code == "zho" || code == "zh" || strings.HasPrefix(code, "zh-"):
		switch {
		case strings.Contains(code, "hant") || strings.HasSuffix(code, "-tw") || strings.HasSuffix(code, "-hk"):
			return []string{"cht"}
		case strings.Contains(code, "hans") || strings.HasSuffix(code, "-cn"):
			return []string{"chs"}
		}
		return []string{"chs", "cht"}
	}
	return []string{code}
}

// embeddedHint reports where the names say there is a built-in subtitle.
func embeddedHint(itemName string, dirs []string, features []file) string {
	if hintRe.MatchString(itemName) {
		return itemName
	}
	for _, d := range dirs {
		if hintRe.MatchString(path.Base(d)) {
			return d
		}
	}
	for _, f := range features {
		if hintRe.MatchString(f.Name) {
			return f.Name
		}
	}
	return ""
}
