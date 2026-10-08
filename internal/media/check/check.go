// Package check inspects works already stored as media items and reports
// what is missing: episodes that never arrived, files whose episode number
// can't be read, and subtitles (external files, or built into the video).
// It lists storage through the same Lister the plan/apply flow uses and takes
// the expected episodes from TMDB or Bangumi. It writes nothing.
package check

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/media/apply"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
)

// Item is a stored media item to check.
type Item struct {
	Path        string
	Kind        string
	Locked      bool
	ExternalIDs map[string]string
}

// ItemSource finds the items at and below root.
type ItemSource interface {
	Items(ctx context.Context, root string) ([]Item, error)
}

// Prober reads the tracks of one video's head.
type Prober interface {
	Probe(ctx context.Context, path string) ([]Track, error)
}

// Flags of a result, shown as the status of the work.
type Flag int

const (
	FlagMissing Flag = 1 << iota // episodes missing
	FlagParse                    // episode numbers unreadable
	FlagNoSubs                   // no (or too few) subtitles
	FlagHint                     // names hint at built-in subtitles, not probed
	FlagFailed                   // listing failed
)

// Result is the finding for one work.
type Result struct {
	Item  Item
	Flags Flag

	Features int
	Matched  int      // features whose episode number was read
	Unparsed []string // feature names without a readable episode number
	Missing  map[int][]int
	Dups     []string // "S01E03: a.mkv, b.mkv"
	Extra    []string // episodes beyond what is expected
	Unaired  int

	Expected     int    // number of expected episodes, 0 = none known
	ExpectSource string // tmdb | bgm | rule | inferred | movie
	ExpectNote   string // why the expectation is weak, or a provider error

	Subs       SubReport
	ParseRatio float64 // share of features whose number couldn't be read
	Errors     []string
	Calls      int
}

// SubReport is the subtitle finding.
type SubReport struct {
	Mode       string
	External   int            // subtitle files found
	Orphans    int            // subtitle files that match no feature
	Covered    int            // features with an external subtitle
	Langs      map[string]int // external subtitle files by language
	Hint       string         // where the names mention built-in subtitles
	Probed     bool
	ProbedFile string
	Tracks     []Track // subtitle tracks of the probed file
	ProbeNote  string
	MissingLng []string
}

// Checker runs the checks.
type Checker struct {
	Items     ItemSource
	Lister    apply.Lister
	Providers *scraper.Set // nil: only rules and inference give an expectation
	Prober    Prober       // nil: embedded subtitles are never probed
	Rules     Rules
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

func (c *Checker) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Run checks every work under root, one after the other.
func (c *Checker) Run(ctx context.Context, root string) ([]Result, error) {
	items, err := c.Items.Items(ctx, root)
	if err != nil {
		return nil, err
	}
	var todo []Item
	for _, it := range items {
		if it.Kind == "ignore" || it.Kind == "collection" {
			continue
		}
		todo = append(todo, it)
	}
	sort.Slice(todo, func(i, j int) bool { return todo[i].Path < todo[j].Path })
	c.logf("%d works to check (%d items under %s)", len(todo), len(items), root)
	var out []Result
	for i, it := range todo {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		c.logf("[%d/%d] %s", i+1, len(todo), it.Path)
		out = append(out, c.checkOne(ctx, it))
	}
	return out, nil
}

type feat struct {
	file
	season, ep int // 0 = unknown
	hasEp      bool
	exact      bool // the season came from the name, not guessed
	subs       int
}

func (c *Checker) checkOne(ctx context.Context, it Item) Result {
	res := Result{Item: it}
	var cr *compiled
	if r, ok := c.Rules[strings.TrimRight(it.Path, "/")]; ok {
		var err error
		if cr, err = compileRule(r); err != nil {
			res.Errors = append(res.Errors, "rule: "+err.Error())
			res.Flags |= FlagFailed
			return res
		}
	}
	if cr == nil {
		cr = &compiled{mode: "any"}
	}

	ls := collect(ctx, c.Lister, it.Path, cr)
	res.Calls = ls.Calls
	res.Errors = append(res.Errors, ls.Errs...)
	if len(ls.Errs) > 0 && len(ls.Features) == 0 {
		res.Flags |= FlagFailed
		return res
	}
	res.Features = len(ls.Features)

	movie := it.Kind == "movie" || strings.HasPrefix(it.ExternalIDs[scraper.ProviderTMDB], "movie/")
	feats := make([]*feat, 0, len(ls.Features))
	for _, f := range ls.Features {
		ft := &feat{file: f}
		if !movie {
			ft.season, ft.ep, ft.hasEp = episodeOf(cr, f.Name)
			ft.exact = ft.season > 0
			if ft.hasEp && ft.season == 0 {
				ft.season = seasonOfDir(f.Dir)
				ft.exact = ft.season > 0
			}
		}
		feats = append(feats, ft)
	}
	sort.Slice(feats, func(i, j int) bool { return feats[i].Rel < feats[j].Rel })

	if movie {
		c.checkMovie(&res, feats)
	} else {
		c.checkEpisodes(ctx, &res, it, cr, feats)
	}
	c.checkSubs(ctx, &res, it, cr, feats, ls.Subs, ls.Single)
	return res
}

func (c *Checker) checkMovie(res *Result, feats []*feat) {
	res.ExpectSource, res.Expected = "movie", 1
	res.Matched = len(feats)
	if len(feats) == 0 {
		res.Flags |= FlagMissing
		res.Missing = map[int][]int{1: {1}}
	} else if len(feats) > 1 {
		names := make([]string, len(feats))
		for i, f := range feats {
			names[i] = f.Name
		}
		res.ExpectNote = fmt.Sprintf("%d main videos (versions or parts?): %s", len(feats), strings.Join(names, ", "))
	}
}

// expectation is what the work should contain: season -> episode numbers.
type expectation struct {
	eps    map[int]map[int]bool
	source string
	note   string
	unaird int
}

func (c *Checker) checkEpisodes(ctx context.Context, res *Result, it Item, cr *compiled, feats []*feat) {
	// Seasons the names state, and the one to give files that state none.
	named := map[int]bool{}
	for _, f := range feats {
		if f.hasEp && f.exact {
			named[f.season] = true
		}
	}
	def := 1
	if len(named) == 1 {
		for s := range named {
			def = s
		}
	}

	exp := c.expectFor(ctx, it, cr, named, def)
	if exp.source == "bgm" {
		// Bangumi has no seasons: numbers run through one list.
		for _, f := range feats {
			f.season = 1
		}
		def = 1
	}
	res.ExpectNote = exp.note
	res.Unaired = exp.unaird

	byKey := map[[2]int][]string{}
	for _, f := range feats {
		if !f.hasEp {
			res.Unparsed = append(res.Unparsed, f.Rel)
			continue
		}
		if f.season == 0 {
			f.season = def
		}
		res.Matched++
		k := [2]int{f.season, f.ep}
		byKey[k] = append(byKey[k], f.Name)
	}
	if res.Features > 0 {
		res.ParseRatio = float64(len(res.Unparsed)) / float64(res.Features)
	}
	if len(res.Unparsed) > 0 && (res.Matched == 0 || res.ParseRatio > 0.3) {
		res.Flags |= FlagParse
	}

	if exp.eps == nil && res.Matched > 0 {
		// no provider answer: assume 1..max of each season found
		exp.eps, exp.source = map[int]map[int]bool{}, "inferred"
		for k := range byKey {
			if exp.eps[k[0]] == nil {
				exp.eps[k[0]] = map[int]bool{}
			}
		}
		for s := range exp.eps {
			hi := 0
			for k := range byKey {
				if k[0] == s && k[1] > hi {
					hi = k[1]
				}
			}
			for e := 1; e <= hi; e++ {
				exp.eps[s][e] = true
			}
		}
		if res.ExpectNote == "" {
			res.ExpectNote = "no provider id or keys: expecting 1..max of what was found, so a missing tail can't be seen"
		}
	}
	res.ExpectSource = exp.source

	res.Missing = map[int][]int{}
	for s, set := range exp.eps {
		res.Expected += len(set)
		for e := range set {
			if len(byKey[[2]int{s, e}]) == 0 {
				res.Missing[s] = append(res.Missing[s], e)
			}
		}
		if len(res.Missing[s]) == 0 {
			delete(res.Missing, s)
		} else {
			sort.Ints(res.Missing[s])
			res.Flags |= FlagMissing
		}
	}
	// A work that has no readable episode at all is a parse problem first.
	if res.Matched == 0 && res.Features > 0 {
		res.Flags &^= FlagMissing
		res.Missing = nil
	}
	var keys [][2]int
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		names := byKey[k]
		if len(names) > 1 {
			res.Dups = append(res.Dups, fmt.Sprintf("S%02dE%02d: %s", k[0], k[1], strings.Join(names, ", ")))
		}
		if exp.eps != nil && !exp.eps[k[0]][k[1]] && exp.source != "inferred" {
			res.Extra = append(res.Extra, fmt.Sprintf("S%02dE%02d", k[0], k[1]))
		}
	}
}

// expectFor picks the expectation: the rule, else the provider the item's ids
// point at (Bangumi first for anime), else none.
func (c *Checker) expectFor(ctx context.Context, it Item, cr *compiled, named map[int]bool, def int) expectation {
	if cr.expect != nil {
		return expectation{eps: cr.expect, source: "rule"}
	}
	if c.Providers == nil {
		return expectation{note: "no provider keys (--env): no expected episode list"}
	}
	tmdbID, bgmID := it.ExternalIDs[scraper.ProviderTMDB], it.ExternalIDs[scraper.ProviderBGM]
	useBGM := bgmID != "" && (it.Kind == "anime" || !strings.HasPrefix(tmdbID, "tv/"))
	switch {
	case useBGM:
		infos, err := c.Providers.BGM.Episodes(ctx, bgmID)
		if err != nil {
			return expectation{note: "bangumi episodes: " + err.Error()}
		}
		return c.fromInfos(infos, "bgm", false)
	case strings.HasPrefix(tmdbID, "tv/") && c.Providers.TMDB != nil:
		tg := &scraper.ScrapeTarget{Kind: "tv"}
		tg.ExternalIDs = map[string]string{scraper.ProviderTMDB: tmdbID}
		if err := c.Providers.TMDB.Fill(ctx, tg); err != nil {
			return expectation{note: "tmdb detail: " + err.Error()}
		}
		want := map[int]bool{}
		for s := range named {
			want[s] = true
		}
		if len(want) == 0 {
			want[def] = true
		}
		var all []scraper.EpisodeInfo
		for _, s := range tg.Seasons {
			if !want[s] {
				continue
			}
			infos, err := c.Providers.TMDB.Season(ctx, tmdbID, s)
			if err != nil {
				return expectation{note: fmt.Sprintf("tmdb season %d: %v", s, err)}
			}
			all = append(all, infos...)
		}
		for s := range want {
			found := false
			for _, n := range tg.Seasons {
				found = found || n == s
			}
			if !found {
				return expectation{note: fmt.Sprintf("tmdb has no season %d for %s", s, tmdbID)}
			}
		}
		return c.fromInfos(all, "tmdb", true)
	case tmdbID != "" || bgmID != "":
		return expectation{note: "no provider for the stored ids (tmdb tv needs --env with TMDB_READ_TOKEN)"}
	}
	return expectation{note: "the item has no tmdb or bgm id"}
}

func (c *Checker) fromInfos(infos []scraper.EpisodeInfo, source string, seasons bool) expectation {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	today := now().Format("2006-01-02")
	e := expectation{eps: map[int]map[int]bool{}, source: source}
	for _, i := range infos {
		if i.Episode <= 0 {
			continue
		}
		if i.Aired != "" && i.Aired > today {
			e.unaird++
			continue
		}
		s := i.Season
		if !seasons {
			s = 1
		}
		if e.eps[s] == nil {
			e.eps[s] = map[int]bool{}
		}
		e.eps[s][i.Episode] = true
	}
	if len(e.eps) == 0 {
		return expectation{note: source + " returned no aired episodes", unaird: e.unaird}
	}
	return e
}

// checkSubs attaches subtitle files to features, decides whether the work is
// covered, and probes the first feature when that is still open.
func (c *Checker) checkSubs(ctx context.Context, res *Result, it Item, cr *compiled, feats []*feat, subs []file, single bool) {
	sr := &res.Subs
	sr.Mode, sr.Langs = cr.mode, map[string]int{}
	if cr.mode == "none" {
		return
	}
	sr.External = len(subs)

	byDir := map[string][]*feat{}
	for _, f := range feats {
		byDir[f.Dir] = append(byDir[f.Dir], f)
	}
	extLangs := map[string]bool{}
	for _, s := range subs {
		var hit *feat
		best := 0
		for _, f := range byDir[s.Dir] {
			st := strings.ToLower(strings.TrimSuffix(f.Name, path.Ext(f.Name)))
			if strings.HasPrefix(strings.ToLower(s.Name), st) && len(st) > best {
				hit, best = f, len(st)
			}
		}
		if hit == nil {
			// a Subs directory: match by the episode number in the name
			if sn, ep, ok := episodeOf(cr, s.Name); ok {
				if sn == 0 {
					sn = seasonOfDir(s.Dir)
				}
				for _, f := range feats {
					if f.hasEp && f.ep == ep && (sn == 0 || f.season == sn || f.season == 0) {
						hit = f
						break
					}
				}
			}
		}
		stem := ""
		if hit != nil {
			stem = strings.TrimSuffix(hit.Name, path.Ext(hit.Name))
		} else if len(feats) == 1 {
			// one work, one video: a lone subtitle belongs to it
			hit = feats[0]
			stem = strings.TrimSuffix(hit.Name, path.Ext(hit.Name))
		}
		if hit == nil {
			sr.Orphans++
		} else {
			hit.subs++
		}
		for _, l := range subLangs(s.Name, stem) {
			sr.Langs[l]++
			extLangs[l] = true
		}
	}
	for _, f := range feats {
		if f.subs > 0 {
			sr.Covered++
		}
	}

	var dirs []string
	seen := map[string]bool{}
	var ff []file
	for _, f := range feats {
		ff = append(ff, f.file)
		if f.Dir != "" && !seen[f.Dir] {
			seen[f.Dir] = true
			dirs = append(dirs, f.Dir)
		}
	}
	for _, d := range dirs {
		for _, s := range segments(d) {
			if hintRe.MatchString(s) {
				sr.Hint = d
			}
		}
	}
	if sr.Hint == "" {
		sr.Hint = embeddedHint(path.Base(it.Path), nil, ff)
	}

	extFull := len(feats) > 0 && sr.Covered == len(feats)
	embLangs := map[string]bool{}

	if cr.mode != "external" && c.Prober != nil && len(feats) > 0 && !(extFull && langsCovered(cr.langs, extLangs)) {
		first := feats[0]
		for _, f := range feats {
			if f.hasEp && (!first.hasEp || f.season < first.season || (f.season == first.season && f.ep < first.ep)) {
				first = f
			}
		}
		if extOf(first.Name) != "mkv" {
			sr.ProbeNote = "not probed: only mkv is supported (" + extOf(first.Name) + ")"
		} else {
			p := path.Join(it.Path, first.Rel)
			if single {
				p = it.Path
			}
			sr.ProbedFile = first.Rel
			tracks, err := c.Prober.Probe(ctx, p)
			if err != nil {
				sr.ProbeNote = "probe failed: " + err.Error()
			} else {
				sr.Probed = true
				sr.Tracks = SubtitleTracks(tracks)
				for _, t := range sr.Tracks {
					for _, l := range trackLang(t.Lang) {
						embLangs[l] = true
					}
				}
			}
		}
	}

	embOK := sr.Probed && len(sr.Tracks) > 0
	have := map[string]bool{}
	if cr.mode != "embedded" {
		for l := range extLangs {
			have[l] = true
		}
	}
	if cr.mode != "external" {
		for l := range embLangs {
			have[l] = true
		}
	}
	covered := false
	switch cr.mode {
	case "external":
		covered = extFull
	case "embedded":
		covered = embOK
	default:
		covered = extFull || embOK
	}
	for _, l := range cr.langs {
		if !have[l] {
			sr.MissingLng = append(sr.MissingLng, l)
		}
	}
	switch {
	case covered && len(sr.MissingLng) == 0:
	case cr.mode != "external" && sr.Hint != "" && !sr.Probed:
		res.Flags |= FlagHint // the names promise built-in subtitles nobody has looked at
	default:
		res.Flags |= FlagNoSubs
	}
}

func langsCovered(want []string, have map[string]bool) bool {
	for _, l := range want {
		if !have[l] {
			return false
		}
	}
	return true
}
