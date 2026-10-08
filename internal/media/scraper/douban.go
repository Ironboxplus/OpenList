package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const doubanDefaultBase = "https://api.douban.com"

// Douban looks a title up by imdb id on the v2 API and reads the Douban id and
// rating. The api key goes in a POST form, never in the URL.
type Douban struct {
	api  *api
	base string
	key  string
}

func newDouban(cfg Config, hc *http.Client) *Douban {
	return &Douban{api: newAPI(cfg, hc, ProviderDouban, 1), base: cfg.base("douban", doubanDefaultBase), key: cfg.DoubanAPIKey}
}

var doubanSubjectRe = regexp.MustCompile(`(\d{5,})`)

// anyString renders a JSON string or number as a string.
func anyString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func anyFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

// Fill reads t.ExternalIDs["imdb"] and sets ExternalIDs["douban"] and
// Ratings["douban"]. The v2 answer has used both numeric ids and subject URLs
// across versions, so both are accepted.
func (d *Douban) Fill(ctx context.Context, tg *ScrapeTarget) error {
	imdb := tg.ExternalIDs["imdb"]
	if imdb == "" {
		return &Error{Provider: ProviderDouban, Class: ClassOther, Msg: "no imdb id"}
	}
	form := url.Values{"apikey": {d.key}}.Encode()
	u := d.base + "/v2/movie/imdb/" + url.PathEscape(imdb)
	body, err := d.api.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(form))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req, nil
	})
	if err != nil {
		return err
	}
	var r map[string]any
	if err := json.Unmarshal(body, &r); err != nil {
		return d.api.fail(ClassBadResp, 0, "unreadable response: "+err.Error(), err)
	}
	if code, ok := r["code"]; ok && anyFloat(code) != 0 {
		return d.api.fail(ClassOther, 0, fmt.Sprintf("douban code %v: %v", code, r["msg"]), nil)
	}
	id := anyString(r["id"])
	if m := doubanSubjectRe.FindString(id); m != "" {
		id = m
	} else if m := doubanSubjectRe.FindString(anyString(r["alt"])); m != "" {
		id = m
	} else {
		id = ""
	}
	tg.setID(ProviderDouban, id)
	if rating, ok := r["rating"].(map[string]any); ok {
		if avg := anyFloat(rating["average"]); avg > 0 {
			tg.setRating(ProviderDouban, round1(avg))
		}
	}
	if id == "" && tg.Ratings[ProviderDouban] == nil {
		return d.api.fail(ClassNotFound, 0, "no douban record for "+imdb, nil)
	}
	return nil
}
