package scraper

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const omdbDefaultBase = "https://www.omdbapi.com"

// OMDb adds the IMDb and Rotten Tomatoes ratings for an imdb id. It has
// several keys (each is a small daily quota) and picks one at random per
// request; a key that is rejected or out of quota is skipped for that request.
type OMDb struct {
	api  *api
	base string
	keys []string
	intn func(int) int
}

func newOMDb(cfg Config, hc *http.Client) *OMDb {
	intn := cfg.Intn
	if intn == nil {
		intn = rand.Intn
	}
	return &OMDb{api: newAPI(cfg, hc, ProviderOMDb, 1), base: cfg.base("omdb", omdbDefaultBase), keys: cfg.OMDbKeys, intn: intn}
}

// order returns the key indexes in the order to try: a random first pick,
// then the rest as fallbacks.
func (o *OMDb) order() []int {
	n := len(o.keys)
	idx := make([]int, 0, n)
	first := o.intn(n)
	idx = append(idx, first)
	for i := 0; i < n; i++ {
		if i != first {
			idx = append(idx, i)
		}
	}
	return idx
}

type omdbResp struct {
	Response   string `json:"Response"`
	Error      string `json:"Error"`
	IMDbRating string `json:"imdbRating"`
	Ratings    []struct {
		Source string `json:"Source"`
		Value  string `json:"Value"`
	} `json:"Ratings"`
}

// Fill reads t.ExternalIDs["imdb"] and sets Ratings["imdb"] (float) and
// Ratings["rt"] (e.g. "94%") when OMDb has them.
func (o *OMDb) Fill(ctx context.Context, tg *ScrapeTarget) error {
	imdb := tg.ExternalIDs["imdb"]
	if imdb == "" {
		return &Error{Provider: ProviderOMDb, Class: ClassOther, Msg: "no imdb id"}
	}
	if len(o.keys) == 0 {
		return &Error{Provider: ProviderOMDb, Class: ClassNotConfig, Msg: "no key configured"}
	}
	var lastErr error
	for _, i := range o.order() {
		key := o.keys[i]
		u := o.base + "/?" + url.Values{"apikey": {key}, "i": {imdb}}.Encode()
		body, err := o.api.do(ctx, func() (*http.Request, error) { return http.NewRequest(http.MethodGet, u, nil) })
		if err != nil {
			if ClassOf(err) == ClassAuth || ClassOf(err) == ClassRate {
				lastErr = err
				continue
			}
			return err
		}
		var r omdbResp
		if err := json.Unmarshal(body, &r); err != nil {
			return o.api.fail(ClassBadResp, 0, "unreadable response: "+err.Error(), err)
		}
		if !strings.EqualFold(r.Response, "True") {
			msg := r.Error
			low := strings.ToLower(msg)
			switch {
			case strings.Contains(low, "api key") || strings.Contains(low, "limit"):
				lastErr = o.api.fail(ClassAuth, 0, "omdb: "+msg, nil)
				continue
			case strings.Contains(low, "not found"):
				return o.api.fail(ClassNotFound, 0, msg, nil)
			}
			return o.api.fail(ClassOther, 0, msg, nil)
		}
		if f, err := strconv.ParseFloat(r.IMDbRating, 64); err == nil && f > 0 {
			tg.setRating("imdb", f)
		}
		for _, rt := range r.Ratings {
			if rt.Source == "Rotten Tomatoes" && rt.Value != "" {
				tg.setRating("rt", rt.Value)
			}
		}
		return nil
	}
	return lastErr
}
