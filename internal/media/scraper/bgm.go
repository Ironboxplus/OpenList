package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const bgmDefaultBase = "https://api.bgm.tv"

// Bangumi is the bgm.tv v0 API. It needs no key but insists on a descriptive
// User-Agent, which the shared plumbing sets to openlist-media/<version>.
type Bangumi struct {
	api  *api
	base string
}

func newBangumi(cfg Config, hc *http.Client) *Bangumi {
	return &Bangumi{api: newAPI(cfg, hc, ProviderBGM, 2), base: cfg.base("bgm", bgmDefaultBase)}
}

type bgmSubject struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	NameCN  string `json:"name_cn"`
	Date    string `json:"date"`
	Summary string `json:"summary"`
	Rating  struct {
		Score float64 `json:"score"`
	} `json:"rating"`
	Score  float64 `json:"score"` // search results carry the score at top level
	Images struct {
		Large  string `json:"large"`
		Common string `json:"common"`
	} `json:"images"`
	Image string `json:"image"` // search results: single image url
	Tags  []struct {
		Name string `json:"name"`
	} `json:"tags"`
	Platform string `json:"platform"`
}

func (s bgmSubject) poster() string { return firstNonEmpty(s.Images.Large, s.Images.Common, s.Image) }
func (s bgmSubject) score() float64 {
	if s.Rating.Score > 0 {
		return s.Rating.Score
	}
	return s.Score
}

func (b *Bangumi) call(ctx context.Context, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	raw, err := b.api.do(ctx, func() (*http.Request, error) {
		var rd *bytes.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		} else {
			rd = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(method, b.base+path, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return b.api.fail(ClassBadResp, 0, "unreadable response: "+err.Error(), err)
	}
	return nil
}

func (b *Bangumi) toCandidate(s bgmSubject, query string, queryYear int) Candidate {
	sim := TitleSimilarity(query, s.NameCN)
	if s2 := TitleSimilarity(query, s.Name); s2 > sim {
		sim = s2
	}
	title := firstNonEmpty(s.NameCN, s.Name)
	return Candidate{
		Provider: ProviderBGM, ID: strconv.Itoa(s.ID), Kind: "anime",
		Title: title, OriginalTitle: s.Name, Year: yearOf(s.Date), PosterURL: s.poster(),
		Score: scoreParts(sim, queryYear, yearOf(s.Date), float32(s.score())),
	}
}

// Search finds anime subjects (type 2) by keyword, scored against q.
func (b *Bangumi) Search(ctx context.Context, q SearchQuery) ([]Candidate, error) {
	if strings.TrimSpace(q.Q) == "" {
		return nil, nil
	}
	var resp struct {
		Data []bgmSubject `json:"data"`
	}
	body := map[string]any{"keyword": q.Q, "filter": map[string]any{"type": []int{2}}}
	if err := b.call(ctx, http.MethodPost, "/v0/search/subjects?limit=10", body, &resp); err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(resp.Data))
	for _, s := range resp.Data {
		out = append(out, b.toCandidate(s, q.Q, q.Year))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// Identify tries each cleaned term until one scores strongly, and returns
// everything seen, best first.
func (b *Bangumi) Identify(ctx context.Context, q IdentifyQuery) ([]Candidate, error) {
	pool := map[string]Candidate{}
	terms := append([]SearchTerm(nil), q.Terms...)
	sort.SliceStable(terms, func(i, j int) bool { return terms[i].Confidence > terms[j].Confidence })
	year := q.Year
	if year == 0 && len(terms) > 0 {
		year = atoiSafe(terms[0].Year)
	}
	tried := 0
	var firstErr error
	for _, t := range terms {
		if tried >= 5 {
			break
		}
		tried++
		cs, err := b.Search(ctx, SearchQuery{Q: t.Name, Year: year})
		if err != nil {
			switch ClassOf(err) {
			case ClassNetwork, ClassAuth, ClassCanceled, ClassRate:
				return nil, err
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		strong := false
		for _, c := range cs {
			if old, ok := pool[c.ID]; !ok || old.Score < c.Score {
				pool[c.ID] = c
			}
			if c.Score >= StrongMatch {
				strong = true
			}
		}
		if strong {
			break
		}
	}
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
	if len(out) == 0 {
		return nil, firstErr
	}
	return out, nil
}

var bgmYearTagRe = regexp.MustCompile(`^\d{4}(年.*)?$`)

// Fill fetches /v0/subjects/{id} for t.ExternalIDs["bgm"].
func (b *Bangumi) Fill(ctx context.Context, tg *ScrapeTarget) error {
	id := tg.ExternalIDs[ProviderBGM]
	if id == "" {
		return &Error{Provider: ProviderBGM, Class: ClassOther, Msg: "no bgm id"}
	}
	var s bgmSubject
	if err := b.call(ctx, http.MethodGet, "/v0/subjects/"+url.PathEscape(id), nil, &s); err != nil {
		return err
	}
	tg.setID(ProviderBGM, id)
	if tg.Kind == "" {
		tg.Kind = "anime"
	}
	tg.Title = firstNonEmpty(s.NameCN, s.Name)
	tg.OriginalTitle = s.Name
	tg.Year = yearOf(s.Date)
	tg.Overview = strings.TrimSpace(s.Summary)
	if sc := s.score(); sc > 0 {
		tg.setRating(ProviderBGM, round1(sc))
	}
	tg.PosterURL = s.poster()
	tg.Genres = tg.Genres[:0]
	for _, t := range s.Tags {
		if len(tg.Genres) >= 6 {
			break
		}
		if t.Name == "" || bgmYearTagRe.MatchString(t.Name) {
			continue
		}
		tg.Genres = append(tg.Genres, t.Name)
	}
	return nil
}

// Episodes lists the main episodes (type 0) of a subject. Bangumi has no
// seasons: every episode is season 1 and `sort` is the running number.
func (b *Bangumi) Episodes(ctx context.Context, id string) ([]EpisodeInfo, error) {
	var out []EpisodeInfo
	for offset := 0; offset < 1000; {
		var resp struct {
			Total int `json:"total"`
			Data  []struct {
				Sort   float64 `json:"sort"`
				Ep     float64 `json:"ep"`
				Name   string  `json:"name"`
				NameCN string  `json:"name_cn"`
				Desc   string  `json:"desc"`
				Date   string  `json:"airdate"`
			} `json:"data"`
		}
		path := "/v0/episodes?type=0&limit=100&offset=" + strconv.Itoa(offset) + "&subject_id=" + url.QueryEscape(id)
		if err := b.call(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		for _, e := range resp.Data {
			n := int(e.Sort)
			if n == 0 {
				n = int(e.Ep)
			}
			out = append(out, EpisodeInfo{Season: 1, Episode: n, Title: firstNonEmpty(e.NameCN, e.Name), Overview: e.Desc, Aired: e.Date})
		}
		offset += 100
		if len(resp.Data) == 0 || offset >= resp.Total {
			break
		}
	}
	return out, nil
}
