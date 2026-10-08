package scraper

import (
	"context"
	"fmt"
	"strings"
)

// Set holds the providers that are configured. A nil member means that
// provider has no key (Bangumi needs none and is always present).
type Set struct {
	TMDB   *TMDB
	BGM    *Bangumi
	OMDb   *OMDb
	Douban *Douban
	Lang   string
}

// NewSet builds the providers from cfg.
func NewSet(cfg Config) (*Set, error) {
	hc, err := cfg.httpClient()
	if err != nil {
		return nil, err
	}
	s := &Set{BGM: newBangumi(cfg, hc), Lang: cfg.lang()}
	if strings.TrimSpace(cfg.TMDBToken) != "" {
		s.TMDB = newTMDB(cfg, hc)
	}
	var keys []string
	for _, k := range cfg.OMDbKeys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		cfg.OMDbKeys = keys
		s.OMDb = newOMDb(cfg, hc)
	}
	if strings.TrimSpace(cfg.DoubanAPIKey) != "" {
		s.Douban = newDouban(cfg, hc)
	}
	return s, nil
}

// SplitKeys splits a comma separated key list.
func SplitKeys(s string) []string {
	var out []string
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func notConfigured(provider string) error {
	return &Error{Provider: provider, Class: ClassNotConfig, Msg: "not configured (no key)"}
}

// Search is the manual-pick search of one provider.
func (s *Set) Search(ctx context.Context, provider string, q SearchQuery) ([]Candidate, error) {
	switch provider {
	case ProviderTMDB:
		if s.TMDB == nil {
			return nil, notConfigured(provider)
		}
		return s.TMDB.Search(ctx, q)
	case ProviderBGM:
		return s.BGM.Search(ctx, q)
	}
	return nil, fmt.Errorf("unknown provider %q (want tmdb or bgm)", provider)
}

// Identify looks a cleaned name up and returns scored candidates. kind routes
// the sources: anime goes to Bangumi first and falls back to TMDB tv (or movie
// for a theatrical anime); movie and tv go to TMDB.
func (s *Set) Identify(ctx context.Context, kind string, theatrical bool, q IdentifyQuery) ([]Candidate, error) {
	switch kind {
	case "anime":
		cs, err := s.BGM.Identify(ctx, q)
		if err == nil && len(cs) > 0 && cs[0].Score >= StrongMatch {
			return cs, nil
		}
		if s.TMDB == nil {
			return cs, err
		}
		q2 := q
		q2.Kind = "tv"
		if theatrical {
			q2.Kind = "movie"
		}
		tc, terr := s.TMDB.Identify(ctx, q2)
		merged := append(append([]Candidate(nil), cs...), tc...)
		sortCandidates(merged)
		if len(merged) == 0 {
			if terr != nil {
				return nil, terr
			}
			return nil, err
		}
		return merged, nil
	default:
		if s.TMDB == nil {
			return nil, notConfigured(ProviderTMDB)
		}
		q.Kind = kind
		return s.TMDB.Identify(ctx, q)
	}
}

func sortCandidates(cs []Candidate) {
	// insertion sort: lists are tiny and it is stable
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].Score > cs[j-1].Score; j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}

// Fetch fills a target from provider/id and then enriches it: with an imdb id
// OMDb adds the IMDb and Rotten Tomatoes ratings, and Douban its id and
// rating. The enrichment is best effort; its failures come back as warnings.
func (s *Set) Fetch(ctx context.Context, provider, id, kind string) (*ScrapeTarget, []string, error) {
	tg := &ScrapeTarget{Kind: kind}
	tg.setID(provider, id)
	switch provider {
	case ProviderTMDB:
		if s.TMDB == nil {
			return nil, nil, notConfigured(provider)
		}
		if err := s.TMDB.Fill(ctx, tg); err != nil {
			return nil, nil, err
		}
		if kind == "anime" {
			tg.Kind = "anime"
		}
	case ProviderBGM:
		if err := s.BGM.Fill(ctx, tg); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, fmt.Errorf("unknown provider %q (want tmdb or bgm)", provider)
	}
	var warns []string
	if tg.ExternalIDs["imdb"] != "" {
		if s.OMDb != nil {
			if err := s.OMDb.Fill(ctx, tg); err != nil {
				warns = append(warns, err.Error())
			}
		}
		if s.Douban != nil {
			if err := s.Douban.Fill(ctx, tg); err != nil {
				warns = append(warns, err.Error())
			}
		}
	}
	return tg, warns, nil
}

// Episodes returns the episode list of a fetched target: every season for a
// TMDB tv show, the single list for a Bangumi subject.
func (s *Set) Episodes(ctx context.Context, tg *ScrapeTarget) ([]EpisodeInfo, error) {
	if id := tg.ExternalIDs[ProviderTMDB]; id != "" && strings.HasPrefix(id, "tv/") && s.TMDB != nil {
		var all []EpisodeInfo
		for _, n := range tg.Seasons {
			eps, err := s.TMDB.Season(ctx, id, n)
			if err != nil {
				return all, err
			}
			all = append(all, eps...)
		}
		return all, nil
	}
	if id := tg.ExternalIDs[ProviderBGM]; id != "" {
		return s.BGM.Episodes(ctx, id)
	}
	return nil, nil
}

// ResolveTMDB turns an inline id tag into a TMDB id with its type ("tv/39218").
// A tmdb number has no type, so the hint ("tv" or "movie") is tried first and
// the other type second; an imdb id goes through TMDB's /find, preferring the
// hint's type when it matches both. The tmdb number wins when both are given.
func (s *Set) ResolveTMDB(ctx context.Context, hint, tmdb, imdb string) (string, error) {
	if s.TMDB == nil {
		return "", notConfigured(ProviderTMDB)
	}
	if hint != "tv" {
		hint = "movie"
	}
	if tmdb != "" {
		other := map[string]string{"tv": "movie", "movie": "tv"}[hint]
		err := s.TMDB.Exists(ctx, hint, tmdb)
		if err == nil {
			return hint + "/" + tmdb, nil
		}
		if ClassOf(err) != ClassNotFound {
			return "", err
		}
		if err2 := s.TMDB.Exists(ctx, other, tmdb); err2 == nil {
			return other + "/" + tmdb, nil
		} else if ClassOf(err2) != ClassNotFound {
			return "", err2
		}
		if imdb == "" {
			return "", err
		}
	}
	ids, err := s.TMDB.FindIMDb(ctx, imdb)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", &Error{Provider: ProviderTMDB, Class: ClassNotFound, Msg: "no tmdb entry for " + imdb}
	}
	for _, id := range ids {
		if strings.HasPrefix(id, hint+"/") {
			return id, nil
		}
	}
	return ids[0], nil
}
