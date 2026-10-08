// Package scraper talks to the metadata providers (TMDB, Bangumi, OMDb, Douban)
// and holds the title-cleaning logic ported from op/dev-media. It knows
// nothing about the database or the file system: callers hand it a
// ScrapeTarget and get the same struct back filled in.
package scraper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

// Version is what the Bangumi User-Agent carries: openlist-media/<Version>.
var Version = "dev"

// Provider names, as used in plan files, candidates and external ids.
const (
	ProviderTMDB   = "tmdb"
	ProviderBGM    = "bgm"
	ProviderOMDb   = "omdb"
	ProviderDouban = "douban"
)

// ScrapeTarget is the only carrier between callers and providers. Kind and
// Year are hints on the way in; everything else is filled by a provider, which
// writes only the fields it has.
type ScrapeTarget struct {
	Kind          string // movie | tv | anime
	Title         string
	OriginalTitle string
	Year          int
	ExternalIDs   map[string]string // tmdb "tv/890", imdb "tt..", bgm "265", douban "1291843"
	Ratings       map[string]any    // tmdb/imdb/douban/bgm float64, rt string
	Genres        []string
	Overview      string
	PosterURL     string
	BackdropURL   string
	Seasons       []int // season numbers of a tv show (TMDB)
}

func (t *ScrapeTarget) setID(provider, id string) {
	if id == "" {
		return
	}
	if t.ExternalIDs == nil {
		t.ExternalIDs = map[string]string{}
	}
	t.ExternalIDs[provider] = id
}

func (t *ScrapeTarget) setRating(provider string, v any) {
	if t.Ratings == nil {
		t.Ratings = map[string]any{}
	}
	t.Ratings[provider] = v
}

// EpisodeInfo is one episode of a season.
type EpisodeInfo struct {
	Season   int
	Episode  int
	Title    string
	Overview string
	StillURL string
	Aired    string // air date YYYY-MM-DD, "" = unknown
}

// Candidate is one search hit.
type Candidate struct {
	Provider      string  `json:"provider"`
	ID            string  `json:"id"`
	Kind          string  `json:"kind,omitempty"` // movie | tv | anime
	Title         string  `json:"title"`
	OriginalTitle string  `json:"original_title,omitempty"`
	Year          int     `json:"year"`
	PosterURL     string  `json:"poster_url"`
	Score         float32 `json:"score"`
}

// Error classes, for callers that react differently (the server reports
// "network" so the UI can say "use the CLI on a PC").
const (
	ClassNetwork   = "network"
	ClassAuth      = "auth"
	ClassRate      = "rate_limit"
	ClassNotFound  = "not_found"
	ClassBadResp   = "bad_response"
	ClassCanceled  = "canceled"
	ClassNotConfig = "not_configured"
	ClassOther     = "other"
)

// Error is a provider failure. Its message never contains a key.
type Error struct {
	Provider string
	Class    string
	Status   int
	Msg      string
	Err      error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %s", e.Provider, e.Msg)
	if e.Status != 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// ClassOf names the class of err; unknown errors are "other".
func ClassOf(err error) string {
	if err == nil {
		return ""
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Class
	}
	if classifyTransport(err) != "" {
		return classifyTransport(err)
	}
	return ClassOther
}

func classifyTransport(err error) string {
	if errors.Is(err, context.Canceled) {
		return ClassCanceled
	}
	var ne net.Error
	var oe *net.OpError
	var de *net.DNSError
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) || errors.As(err, &oe) || errors.As(err, &de) {
		return ClassNetwork
	}
	return ""
}

var (
	maskKVRe     = regexp.MustCompile(`(?i)\b(api_?key|apikey|access_token|token)=[^&\s"']*`)
	maskBearerRe = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
)

// MaskSecrets hides api keys and Bearer tokens in s, plus any literal secret
// passed in. Everything that reaches a log line or an error message goes
// through it.
func MaskSecrets(s string, secrets ...string) string {
	s = maskKVRe.ReplaceAllString(s, "${1}=***")
	s = maskBearerRe.ReplaceAllString(s, "Bearer ***")
	for _, sec := range secrets {
		if len(sec) >= 6 {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// Config configures a Set of providers. Zero values are fine; a provider
// whose key is empty is simply absent.
type Config struct {
	TMDBToken    string   // v4 Bearer token (starts with "eyJ") or v3 api_key
	OMDbKeys     []string // one is picked at random per request
	DoubanAPIKey string
	Language     string // default zh-CN
	Timeout      time.Duration
	Proxy        string // outbound proxy URL for every request
	// BaseURLs overrides provider endpoints (tests). Keys: tmdb (including
	// the /3 prefix), bgm, omdb, douban, tmdb_image.
	BaseURLs map[string]string
	// RatePerSec overrides the per-provider limits (tests).
	RatePerSec map[string]float64
	// HTTPClient replaces the client built from Timeout/Proxy (tests).
	HTTPClient *http.Client
	// Intn picks an OMDb key; defaults to math/rand.
	Intn func(n int) int
	// RetryDelay is the pause before the single retry of a 429/5xx answer.
	RetryDelay time.Duration
}

func (c Config) lang() string {
	if c.Language == "" {
		return "zh-CN"
	}
	return c.Language
}

func (c Config) base(key, def string) string {
	if u := c.BaseURLs[key]; u != "" {
		return strings.TrimRight(u, "/")
	}
	return def
}

func (c Config) secrets() []string {
	s := []string{c.TMDBToken, c.DoubanAPIKey}
	return append(s, c.OMDbKeys...)
}

func (c Config) httpClient() (*http.Client, error) {
	if c.HTTPClient != nil {
		return c.HTTPClient, nil
	}
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	}
	if c.Proxy != "" {
		pu, err := url.Parse(c.Proxy)
		if err != nil || pu.Host == "" {
			return nil, fmt.Errorf("invalid proxy URL")
		}
		tr.Proxy = http.ProxyURL(pu)
	} else {
		tr.Proxy = http.ProxyFromEnvironment
	}
	to := c.Timeout
	if to <= 0 {
		to = 15 * time.Second
	}
	return &http.Client{Transport: tr, Timeout: to}, nil
}

// maxBody caps one provider response.
const maxBody = 8 << 20

// api is the shared plumbing: one rate limiter per provider, a size cap, a
// single retry on 429/5xx, error classification and key masking.
type api struct {
	name       string
	hc         *http.Client
	lim        *rate.Limiter
	ua         string
	secrets    []string
	retryDelay time.Duration
}

func newAPI(cfg Config, hc *http.Client, name string, defRate float64) *api {
	r := defRate
	if v, ok := cfg.RatePerSec[name]; ok && v > 0 {
		r = v
	}
	burst := int(r)
	if burst < 1 {
		burst = 1
	}
	rd := cfg.RetryDelay
	if rd == 0 {
		rd = time.Second
	}
	return &api{
		name: name, hc: hc, lim: rate.NewLimiter(rate.Limit(r), burst),
		ua: "openlist-media/" + Version, secrets: cfg.secrets(), retryDelay: rd,
	}
}

func (a *api) mask(s string) string { return MaskSecrets(s, a.secrets...) }

func (a *api) fail(class string, status int, msg string, err error) *Error {
	return &Error{Provider: a.name, Class: class, Status: status, Msg: a.mask(msg), Err: err}
}

// do sends the request built by mk (rebuilt for a retry) and returns the body
// of a 2xx answer.
func (a *api) do(ctx context.Context, mk func() (*http.Request, error)) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := a.lim.Wait(ctx); err != nil {
			return nil, a.fail(ClassCanceled, 0, "canceled while waiting for the rate limit", err)
		}
		req, err := mk()
		if err != nil {
			return nil, a.fail(ClassOther, 0, "build request: "+err.Error(), err)
		}
		req = req.WithContext(ctx)
		req.Header.Set("User-Agent", a.ua)
		log.Debugf("[media/%s] %s %s", a.name, req.Method, a.mask(req.URL.String()))
		resp, err := a.hc.Do(req)
		if err != nil {
			// *url.Error carries the full URL, key included: keep only the cause.
			var ue *url.Error
			cause := err
			if errors.As(err, &ue) {
				cause = ue.Err
			}
			class := classifyTransport(cause)
			if class == "" {
				class = ClassOther
			}
			log.Warnf("[media/%s] request failed: %s", a.name, a.mask(cause.Error()))
			return nil, a.fail(class, 0, "request failed: "+cause.Error(), cause)
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if rerr != nil {
			return nil, a.fail(ClassNetwork, resp.StatusCode, "read response: "+rerr.Error(), rerr)
		}
		if len(body) > maxBody {
			return nil, a.fail(ClassBadResp, resp.StatusCode, "response too large", nil)
		}
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return body, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			class := ClassOther
			if resp.StatusCode == http.StatusTooManyRequests {
				class = ClassRate
			}
			lastErr = a.fail(class, resp.StatusCode, "server answered "+http.StatusText(resp.StatusCode), nil)
			if attempt == 0 {
				select {
				case <-time.After(a.retryDelay):
					continue
				case <-ctx.Done():
					return nil, a.fail(ClassCanceled, 0, "canceled", ctx.Err())
				}
			}
			return nil, lastErr
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return nil, a.fail(ClassAuth, resp.StatusCode, "key rejected or missing: "+snippet(body), nil)
		case resp.StatusCode == http.StatusNotFound:
			return nil, a.fail(ClassNotFound, resp.StatusCode, "not found", nil)
		default:
			return nil, a.fail(ClassOther, resp.StatusCode, snippet(body), nil)
		}
	}
	return nil, lastErr
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}
