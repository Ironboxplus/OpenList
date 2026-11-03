// Staged copy of our TMDB scraper (PLAN_MERGE_UPSTREAM_BE 2.3); folded in by step 4.3.

package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	tmdbDefaultBase  = "https://api.themoviedb.org/3"
	tmdbDefaultImage = "https://image.tmdb.org/t/p"
	// tmdbMaxAttempts bounds the searches spent on one name.
	tmdbMaxAttempts = 14
)

// TMDB is the TMDB v3 API. The token is either a v4 read token (JWT, starts
// with "eyJ", sent as a Bearer header) or a v3 api key (sent as api_key).
type TMDB struct {
	api   *api
	base  string
	image string
	token string
	lang  string
}

func newTMDB(cfg Config, hc *http.Client) *TMDB {
	return &TMDB{
		api:   newAPI(cfg, hc, ProviderTMDB, 4),
		base:  cfg.base("tmdb", tmdbDefaultBase),
		image: cfg.base("tmdb_image", tmdbDefaultImage),
		token: strings.TrimSpace(cfg.TMDBToken),
		lang:  cfg.lang(),
	}
}

func (t *TMDB) isBearer() bool { return strings.HasPrefix(t.token, "eyJ") }

func (t *TMDB) get(ctx context.Context, path string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	if !t.isBearer() {
		q.Set("api_key", t.token)
	}
	u := t.base + path + "?" + q.Encode()
	body, err := t.api.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if t.isBearer() {
			req.Header.Set("Authorization", "Bearer "+t.token)
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return t.api.fail(ClassBadResp, 0, "unreadable response: "+err.Error(), err)
	}
	return nil
}

// SearchQuery is one search request.
type SearchQuery struct {
	Q    string
	Kind string // movie | tv | multi (default movie)
	Year int
	Lang string // "" = provider default; "none" = send no language
}

type tmdbSearchResp struct {
	Results []tmdbHit `json:"results"`
}

func (t *TMDB) rawSearch(ctx context.Context, q SearchQuery) ([]tmdbHit, error) {
	if strings.TrimSpace(q.Q) == "" {
		return nil, nil
	}
	ep := q.Kind
	if ep == "" || ep == "anime" {
		ep = "movie"
	}
	v := url.Values{}
	v.Set("query", q.Q)
	v.Set("include_adult", "false")
	v.Set("page", "1")
	switch q.Lang {
	case "none":
	case "":
		v.Set("language", t.lang)
	default:
		v.Set("language", q.Lang)
	}
	if q.Year > 0 {
		switch ep {
		case "movie":
			v.Set("primary_release_year", strconv.Itoa(q.Year))
		case "tv":
			v.Set("first_air_date_year", strconv.Itoa(q.Year))
		}
	}
	var resp tmdbSearchResp
	if err := t.get(ctx, "/search/"+ep, v, &resp); err != nil {
		return nil, err
	}
	out := resp.Results[:0]
	for _, h := range resp.Results {
		if (ep == "movie" || ep == "tv") && h.MediaType == "" {
			h.MediaType = ep
		}
		if h.MediaType == "movie" || h.MediaType == "tv" {
			out = append(out, h)
		}
	}
	return out, nil
}

func (t *TMDB) toCandidate(h tmdbHit, score float32) Candidate {
	title, orig := h.Title, ""
	if title == "" {
		title = h.Name
	}
	date := h.ReleaseDate
	if date == "" {
		date = h.FirstAirDate
	}
	poster := ""
	if h.PosterPath != "" {
		poster = t.image + "/w342" + h.PosterPath
	}
	return Candidate{
		Provider: ProviderTMDB, ID: h.MediaType + "/" + strconv.Itoa(h.ID), Kind: h.MediaType,
		Title: title, OriginalTitle: orig, Year: yearOf(date), PosterURL: poster, Score: score,
	}
}

// Search is a plain search for the manual-pick UI: results scored against q
// and sorted best first.
func (t *TMDB) Search(ctx context.Context, q SearchQuery) ([]Candidate, error) {
	hits, err := t.rawSearch(ctx, q)
	if err != nil {
		return nil, err
	}
	year := ""
	if q.Year > 0 {
		year = strconv.Itoa(q.Year)
	}
	out := make([]Candidate, 0, len(hits))
	for _, h := range hits {
		out = append(out, t.toCandidate(h, scoreMatch(q.Q, year, h)))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > 10 {
		out = out[:10]
	}
	return out, nil
}

// IdentifyQuery is a cleaned name to look up.
type IdentifyQuery struct {
	Terms   []SearchTerm
	Kind    string // movie | tv
	Year    int
	English string
	// Content are names found in what the directory holds (the English name of its
	// video file or season folders, with a year): a second opinion on a title
	// the folder only spells in Chinese. See ContentName.
	Content []ContentName
}

// ContentName is a title read from inside a directory.
type ContentName struct {
	Name string
	Year int // 0 = none
}

// Identify walks the dev-media fallback ladder: each term with and without
// year, then multi-search without language, then English keywords, then a
// lower bar. It stops at the first strong hit (score >= 0.6) and returns
// every distinct hit seen, best first. A network or auth failure aborts at
// once instead of repeating the request 14 times.
func (t *TMDB) Identify(ctx context.Context, q IdentifyQuery) ([]Candidate, error) {
	kind := q.Kind
	if kind != "tv" {
		kind = "movie"
	}
	pool := map[string]Candidate{}
	attempts := 0
	seen := map[string]bool{}
	var firstErr error
	strong := false
	found := map[string]map[string]bool{} // candidate id -> the queries that returned it

	try := func(ep, query string, year int, lang string) (stop bool, err error) {
		if attempts >= tmdbMaxAttempts || strings.TrimSpace(query) == "" {
			return false, nil
		}
		key := fmt.Sprintf("%s|%s|%d|%s", ep, query, year, lang)
		if seen[key] {
			return false, nil
		}
		seen[key] = true
		attempts++
		hits, err := t.rawSearch(ctx, SearchQuery{Q: query, Kind: ep, Year: year, Lang: lang})
		if err != nil {
			switch ClassOf(err) {
			case ClassNetwork, ClassAuth, ClassCanceled, ClassRate:
				return true, err
			}
			if firstErr == nil {
				firstErr = err
			}
			return false, nil
		}
		ys := ""
		if year > 0 {
			ys = strconv.Itoa(year)
		}
		for i, h := range hits {
			if i >= 5 {
				break
			}
			sc := scoreMatch(query, ys, h)
			c := t.toCandidate(h, sc)
			if found[c.ID] == nil {
				found[c.ID] = map[string]bool{}
			}
			found[c.ID][query] = true
			if old, ok := pool[c.ID]; !ok || old.Score < sc {
				pool[c.ID] = c
			}
			if sc >= matchConfidenceThreshold {
				strong = true
			}
		}
		return strong, nil
	}

	finish := func(err error) ([]Candidate, error) {
		out := make([]Candidate, 0, len(pool))
		for _, c := range pool {
			out = append(out, c)
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Score != out[j].Score {
				return out[i].Score > out[j].Score
			}
			return out[i].ID < out[j].ID
		})
		if len(out) == 0 && err == nil {
			err = firstErr
		}
		if len(out) > 0 {
			return out, nil
		}
		return nil, err
	}

	// finishAll adds the second opinion of the directory content before it answers.
	finishAll := func(err error) ([]Candidate, error) {
		if err == nil {
			err = t.identifyByContent(ctx, q, kind, try, pool, found)
		}
		return finish(err)
	}

	terms := append([]SearchTerm(nil), q.Terms...)
	sort.SliceStable(terms, func(i, j int) bool { return terms[i].Confidence > terms[j].Confidence })
	year := q.Year
	if year == 0 && len(terms) > 0 {
		year = atoiSafe(terms[0].Year)
	}

	for _, c := range terms {
		if year > 0 {
			if stop, err := try(kind, c.Name, year, c.Lang); stop {
				return finishAll(err)
			}
		}
		if stop, err := try(kind, c.Name, 0, c.Lang); stop {
			return finishAll(err)
		}
	}
	// Nothing strong yet: multi endpoint, any language, for the first terms.
	for i, c := range terms {
		if i >= 3 {
			break
		}
		if stop, err := try("multi", c.Name, 0, "none"); stop {
			return finishAll(err)
		}
	}
	if kw := extractEnglishKeywords(q.English); kw != "" && kw != q.English {
		for _, ep := range []string{kind, "multi"} {
			if stop, err := try(ep, kw, 0, "none"); stop {
				return finishAll(err)
			}
		}
	}
	return finishAll(nil)
}

type tmdbDetail struct {
	ID            int     `json:"id"`
	Title         string  `json:"title"`
	Name          string  `json:"name"`
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	OriginalLang  string  `json:"original_language"`
	Overview      string  `json:"overview"`
	PosterPath    string  `json:"poster_path"`
	BackdropPath  string  `json:"backdrop_path"`
	ReleaseDate   string  `json:"release_date"`
	FirstAirDate  string  `json:"first_air_date"`
	VoteAverage   float64 `json:"vote_average"`
	IMDbID        string  `json:"imdb_id"`
	Genres        []struct {
		Name string `json:"name"`
	} `json:"genres"`
	ExternalIDs struct {
		IMDbID string `json:"imdb_id"`
	} `json:"external_ids"`
	Seasons []struct {
		SeasonNumber int `json:"season_number"`
	} `json:"seasons"`
	// AlternativeTitles: a show lists them under "results", a film under "titles".
	AlternativeTitles struct {
		Results []tmdbAltTitle `json:"results"`
		Titles  []tmdbAltTitle `json:"titles"`
	} `json:"alternative_titles"`
}

type tmdbAltTitle struct {
	Country string `json:"iso_3166_1"`
	Title   string `json:"title"`
}

// regionTitle is the name people in the configured language's region know a
// work by, for a work TMDB has no translation of: it then answers in the
// original language ("Love, Death & Robots" for zh-CN) while its alternative
// titles for CN hold "爱，死亡和机器人". title is what the detail came back
// with. It returns "" when the answer is already localized or no alternative
// fits.
func (t *TMDB) regionTitle(d *tmdbDetail, title, original string) string {
	primary, region, _ := strings.Cut(strings.ReplaceAll(t.lang, "_", "-"), "-")
	if region == "" || strings.EqualFold(primary, "en") || strings.EqualFold(primary, d.OriginalLang) || title != original {
		return ""
	}
	regions := []string{strings.ToUpper(region)}
	switch strings.ToUpper(t.lang) {
	case "ZH-CN":
		regions = append(regions, "SG")
	case "ZH-TW":
		regions = append(regions, "HK")
	}
	alts := append(append([]tmdbAltTitle{}, d.AlternativeTitles.Results...), d.AlternativeTitles.Titles...)
	for _, reg := range regions {
		for _, a := range alts {
			if a.Country == reg && a.Title != "" && a.Title != title {
				return a.Title
			}
		}
	}
	return ""
}

// SplitID turns "tv/890" into ("tv", "890"); a bare id takes the hint kind.
func SplitTMDBID(id, hint string) (kind, num string) {
	if k, n, ok := strings.Cut(id, "/"); ok {
		return k, n
	}
	if hint != "tv" {
		hint = "movie"
	}
	return hint, id
}

// Fill fetches the detail of t.ExternalIDs["tmdb"] ("tv/890" or "movie/550")
// in the configured language, with external ids appended in the same call.
func (t *TMDB) Fill(ctx context.Context, tg *ScrapeTarget) error {
	id := tg.ExternalIDs[ProviderTMDB]
	if id == "" {
		return &Error{Provider: ProviderTMDB, Class: ClassOther, Msg: "no tmdb id"}
	}
	kind, num := SplitTMDBID(id, tg.Kind)
	if kind != "tv" && kind != "movie" {
		return &Error{Provider: ProviderTMDB, Class: ClassOther, Msg: "bad tmdb id " + id}
	}
	v := url.Values{}
	v.Set("language", t.lang)
	v.Set("append_to_response", "external_ids,alternative_titles")
	var d tmdbDetail
	if err := t.get(ctx, "/"+kind+"/"+num, v, &d); err != nil {
		return err
	}
	tg.setID(ProviderTMDB, kind+"/"+num)
	if kind == "tv" {
		tg.Title, tg.OriginalTitle = firstNonEmpty(d.Name, d.Title), firstNonEmpty(d.OriginalName, d.OriginalTitle)
		if alt := t.regionTitle(&d, tg.Title, tg.OriginalTitle); alt != "" {
			tg.Title = alt
		}
		tg.Year = yearOf(d.FirstAirDate)
		if tg.Kind == "" {
			tg.Kind = "tv"
		}
	} else {
		tg.Title, tg.OriginalTitle = firstNonEmpty(d.Title, d.Name), firstNonEmpty(d.OriginalTitle, d.OriginalName)
		if alt := t.regionTitle(&d, tg.Title, tg.OriginalTitle); alt != "" {
			tg.Title = alt
		}
		tg.Year = yearOf(d.ReleaseDate)
		if tg.Kind == "" {
			tg.Kind = "movie"
		}
	}
	tg.Overview = d.Overview
	if imdb := firstNonEmpty(d.ExternalIDs.IMDbID, d.IMDbID); imdb != "" {
		tg.setID("imdb", imdb)
	}
	if d.VoteAverage > 0 {
		tg.setRating(ProviderTMDB, round1(d.VoteAverage))
	}
	tg.Genres = tg.Genres[:0]
	for _, g := range d.Genres {
		tg.Genres = append(tg.Genres, g.Name)
	}
	if d.PosterPath != "" {
		tg.PosterURL = t.image + "/w500" + d.PosterPath
	}
	if d.BackdropPath != "" {
		tg.BackdropURL = t.image + "/w1280" + d.BackdropPath
	}
	tg.Seasons = tg.Seasons[:0]
	for _, s := range d.Seasons {
		if s.SeasonNumber > 0 {
			tg.Seasons = append(tg.Seasons, s.SeasonNumber)
		}
	}
	return nil
}

// Season lists the episodes of one season of a tv show.
func (t *TMDB) Season(ctx context.Context, tvID string, season int) ([]EpisodeInfo, error) {
	_, num := SplitTMDBID(tvID, "tv")
	v := url.Values{}
	v.Set("language", t.lang)
	var resp struct {
		Episodes []struct {
			EpisodeNumber int    `json:"episode_number"`
			Name          string `json:"name"`
			Overview      string `json:"overview"`
			StillPath     string `json:"still_path"`
			AirDate       string `json:"air_date"`
		} `json:"episodes"`
	}
	if err := t.get(ctx, "/tv/"+num+"/season/"+strconv.Itoa(season), v, &resp); err != nil {
		return nil, err
	}
	out := make([]EpisodeInfo, 0, len(resp.Episodes))
	for _, e := range resp.Episodes {
		ei := EpisodeInfo{Season: season, Episode: e.EpisodeNumber, Title: e.Name, Overview: e.Overview, Aired: e.AirDate}
		if e.StillPath != "" {
			ei.StillURL = t.image + "/w300" + e.StillPath
		}
		out = append(out, ei)
	}
	return out, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

// Exists checks that kind ("tv" or "movie") has an entry num. A missing one is
// class not_found.
func (t *TMDB) Exists(ctx context.Context, kind, num string) error {
	var d struct {
		ID int `json:"id"`
	}
	return t.get(ctx, "/"+kind+"/"+url.PathEscape(num), nil, &d)
}

// FindIMDb maps an imdb id to TMDB ids ("movie/550", "tv/890"), movies first.
func (t *TMDB) FindIMDb(ctx context.Context, imdb string) ([]string, error) {
	var resp struct {
		Movies []struct {
			ID int `json:"id"`
		} `json:"movie_results"`
		TV []struct {
			ID int `json:"id"`
		} `json:"tv_results"`
	}
	if err := t.get(ctx, "/find/"+url.PathEscape(imdb), url.Values{"external_source": {"imdb_id"}}, &resp); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range resp.Movies {
		out = append(out, "movie/"+strconv.Itoa(m.ID))
	}
	for _, m := range resp.TV {
		out = append(out, "tv/"+strconv.Itoa(m.ID))
	}
	return out, nil
}

// contentSkipScore is how sure the name search must already be to skip the
// content search: a clean match on title and year needs no second opinion.
const contentSkipScore = 0.85

// identifyByContent searches the English names found inside the directory and
// lets the two searches vouch for each other: a hit that both the folder name
// and a content name return gains, one whose year disagrees with the year the
// files name loses. 回忆三部曲 found by "回忆" is 杀人回忆 (2003); "Memories"
// 1995 finds 回忆三部曲 (MEMORIES, 1995) and the year settles it. The caller
// sees only the final scores. A returned error is fatal (network, auth).
func (t *TMDB) identifyByContent(ctx context.Context, q IdentifyQuery, kind string,
	try func(ep, query string, year int, lang string) (bool, error),
	pool map[string]Candidate, found map[string]map[string]bool) error {
	if len(q.Content) == 0 {
		return nil
	}
	best := float32(0)
	for _, c := range pool {
		best = max(best, c.Score)
	}
	if best >= contentSkipScore {
		return nil
	}
	contentYear := 0
	for i, c := range q.Content {
		if i >= 2 {
			break
		}
		if contentYear == 0 {
			contentYear = c.Year
		}
		if c.Year > 0 {
			if _, err := try(kind, c.Name, c.Year, "zh-CN"); err != nil {
				return err
			}
		}
		if _, err := try(kind, c.Name, 0, "zh-CN"); err != nil {
			return err
		}
	}
	for id, c := range pool {
		sc := c.Score
		if len(found[id]) >= 2 {
			sc += 0.15
		}
		// Only a film has one year to agree on: a show names the years of its later
		// seasons and of whatever sits beside them (食戟之灵 holds a 2021 film).
		if kind == "movie" && contentYear > 0 && c.Year > 0 {
			switch d := absInt(c.Year - contentYear); {
			case d == 0:
				sc += 0.05
			case d > 1:
				sc -= 0.3
			}
		}
		c.Score = min(max(sc, 0), 1)
		pool[id] = c
	}
	return nil
}
