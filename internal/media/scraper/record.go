package scraper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
)

// Recorder wraps the HTTP client of a Set and keeps every search, detail and
// season answer, to be replayed by tests (a fixture of real provider behaviour).
// It is strictly sequential (one request at a time, whatever the callers do),
// stops at a request cap, and never keeps a request header. Before anything is
// written the secrets of the Config are cut out of it, and the write is refused
// if one survives.
type Recorder struct {
	MaxRequests int

	mu       sync.Mutex
	secrets  []string
	hosts    map[string]string // host -> provider
	entries  []RecordedResponse
	requests map[string]int
	total    int
}

// RecordedResponse is one kept answer. URL has no scheme and no key parameter.
type RecordedResponse struct {
	Provider string `json:"provider"`
	// Method is empty for GET. Request is the body of a POST (Bangumi searches are
	// POSTs); it holds no credential.
	Method   string `json:"method,omitempty"`
	Request  string `json:"request,omitempty"`
	URL      string `json:"url"`
	Status   int    `json:"status"`
	Body     string `json:"body"`
}

// maxRequestBody caps the POST body that is kept.
const maxRequestBody = 64 << 10

// ErrRecordLimit is returned (and nothing is sent) once MaxRequests is reached.
var ErrRecordLimit = errors.New("recorder: request limit reached")

// keyParams are query parameters that carry a credential.
var keyParams = []string{"api_key", "apikey", "key", "access_token", "token"}

// Record routes the providers of cfg through a new Recorder and returns it.
func Record(cfg *Config, max int) (*Recorder, error) {
	hc, err := cfg.httpClient()
	if err != nil {
		return nil, err
	}
	r := &Recorder{MaxRequests: max, secrets: cfg.secrets(), hosts: map[string]string{}, requests: map[string]int{}}
	for name, host := range map[string]string{
		"tmdb": "api.themoviedb.org", "bgm": "api.bgm.tv", "omdb": "www.omdbapi.com",
		"douban": "frodo.douban.com", "tmdb_image": "image.tmdb.org",
	} {
		r.hosts[host] = name
	}
	for name, base := range cfg.BaseURLs {
		if u, err := url.Parse(base); err == nil && u.Host != "" {
			r.hosts[u.Host] = name
		}
	}
	next := hc.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	cp := *hc
	cp.Transport = recorderTransport{r: r, next: next}
	cfg.HTTPClient = &cp
	return r, nil
}

type recorderTransport struct {
	r    *Recorder
	next http.RoundTripper
}

func (t recorderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := t.r
	r.mu.Lock()
	defer r.mu.Unlock() // one request at a time
	prov := r.hosts[req.URL.Host]
	if prov == "" {
		prov = req.URL.Host
	}
	if r.MaxRequests > 0 && r.total >= r.MaxRequests {
		return nil, ErrRecordLimit
	}
	r.total++
	r.requests[prov]++
	var sent []byte
	if req.Method == http.MethodPost && req.Body != nil {
		var rerr error
		if sent, rerr = io.ReadAll(req.Body); rerr != nil {
			return nil, rerr
		}
		_ = req.Body.Close()
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(sent))
		req.ContentLength = int64(len(sent))
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil || (req.Method != http.MethodGet && req.Method != http.MethodPost) || prov == "tmdb_image" || len(sent) > maxRequestBody {
		return resp, err
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	_ = resp.Body.Close()
	if rerr != nil {
		return nil, rerr
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) <= maxBody {
		e := RecordedResponse{Provider: prov, URL: cleanURL(req.URL), Status: resp.StatusCode, Body: string(body)}
		if req.Method == http.MethodPost {
			e.Method, e.Request = "POST", string(sent)
		}
		r.entries = append(r.entries, e)
	}
	return resp, nil
}

// cleanURL is host + path + the query without credential parameters, sorted.
func cleanURL(u *url.URL) string {
	q := u.Query()
	for _, k := range keyParams {
		q.Del(k)
	}
	s := u.Host + u.Path
	if enc := q.Encode(); enc != "" {
		s += "?" + enc
	}
	return s
}

// Summary says how many requests went out per provider and how many answers
// are kept. It holds no URL.
func (r *Recorder) Summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var parts []string
	for p, n := range r.requests {
		parts = append(parts, fmt.Sprintf("%s=%d", p, n))
	}
	sort.Strings(parts)
	return fmt.Sprintf("requests=%d (%s) entries=%d", r.total, strings.Join(parts, " "), len(r.entries))
}

// Entries is the number of answers kept.
func (r *Recorder) Entries() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// Marshal renders the kept answers as JSON lines with every secret replaced by
// <redacted>. It fails if a secret is still in the output afterwards.
func (r *Recorder) Marshal() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out bytes.Buffer
	for _, e := range r.entries {
		for _, s := range r.secrets {
			if len(s) < 4 {
				continue
			}
			e.URL = strings.ReplaceAll(e.URL, s, "<redacted>")
			e.Body = strings.ReplaceAll(e.Body, s, "<redacted>")
			e.Request = strings.ReplaceAll(e.Request, s, "<redacted>")
		}
		line, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	for _, s := range r.secrets {
		if len(s) < 4 {
			continue
		}
		escaped, _ := json.Marshal(s)
		if bytes.Contains(out.Bytes(), []byte(s)) || bytes.Contains(out.Bytes(), escaped[1:len(escaped)-1]) {
			return nil, errors.New("recorder: a secret survived the redaction; nothing written")
		}
	}
	return out.Bytes(), nil
}

// WriteFile writes Marshal's output to path (mode 0600), refusing to overwrite.
func (r *Recorder) WriteFile(path string) error {
	data, err := r.Marshal()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
