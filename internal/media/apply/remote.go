package apply

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// API is the OpenList HTTP client the remote implementations use; the CLI's
// client in cmd/sync.go satisfies it.
type API interface {
	Call(method, path string, body, out any) error
	Upload(path, field, filename string, data []byte, out any) error
}

// Remote implements Lister, Writer, ServerScraper and Searcher over the HTTP
// API, for the CLI running on another machine.
type Remote struct{ API API }

type listResp struct {
	Content []struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	} `json:"content"`
	Total int64 `json:"total"`
}

// List calls /api/fs/list (never with refresh).
func (r Remote) List(_ context.Context, p string) ([]identify.Entry, error) {
	var resp listResp
	if err := r.API.Call("POST", "/api/fs/list", map[string]any{"path": p, "page": 1, "per_page": 0}, &resp); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "not a folder") || strings.Contains(msg, "not a directory") {
			return nil, ErrNotDir
		}
		return nil, err
	}
	out := make([]identify.Entry, 0, len(resp.Content))
	for _, c := range resp.Content {
		out = append(out, identify.Entry{Name: c.Name, IsDir: c.IsDir, Size: c.Size})
	}
	return out, nil
}

type mediaListResp struct {
	Content []Existing `json:"content"`
	Total   int64      `json:"total"`
}

// Existing pages through /api/admin/media/list.
func (r Remote) Existing(_ context.Context, prefix string) (map[string]Existing, error) {
	out := map[string]Existing{}
	for page := 1; ; page++ {
		var resp mediaListResp
		q := url.Values{"prefix": {prefix}, "page": {strconv.Itoa(page)}, "per_page": {"1000"}}
		if err := r.API.Call("GET", "/api/admin/media/list?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		for _, e := range resp.Content {
			out[e.Path] = e
		}
		if len(resp.Content) == 0 || int64(page*1000) >= resp.Total {
			return out, nil
		}
	}
}

// UploadAsset posts the image as multipart.
func (r Remote) UploadAsset(_ context.Context, data []byte, srcURL string) (uint, error) {
	name := "image"
	if srcURL != "" {
		if u, err := url.Parse(srcURL); err == nil && u.Path != "" {
			name = u.Path[strings.LastIndex(u.Path, "/")+1:]
		}
	}
	var out struct {
		ID uint `json:"id"`
	}
	if err := r.API.Upload("/api/admin/media/asset?source_url="+url.QueryEscape(srcURL), "file", name, data, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// Set posts to /api/admin/media/set, which is an admin's explicit edit and so
// overwrites a locked item as well.
func (r Remote) Set(_ context.Context, req model.MediaSetReq, _ bool) error {
	return r.API.Call("POST", "/api/admin/media/set", req, nil)
}

// ServerScrape asks the server to scrape provider:id into path.
func (r Remote) ServerScrape(_ context.Context, p, provider, id string, lock, force bool) error {
	return r.API.Call("POST", "/api/admin/media/scrape",
		map[string]any{"path": p, "provider": provider, "id": id, "lock": lock, "force": force}, nil)
}

func (r Remote) search(provider, q, kind string, year int) ([]scraper.Candidate, error) {
	v := url.Values{"provider": {provider}, "q": {q}}
	if kind != "" {
		v.Set("kind", kind)
	}
	if year > 0 {
		v.Set("year", strconv.Itoa(year))
	}
	var cs []scraper.Candidate
	if err := r.API.Call("GET", "/api/admin/media/search?"+v.Encode(), nil, &cs); err != nil {
		return nil, wrapRemoteErr(err)
	}
	return cs, nil
}

// wrapRemoteErr restores the error class the server put in front of its
// message ("network: ..."), so a server that can't reach a provider reads as
// a network failure here too.
func wrapRemoteErr(err error) error {
	msg := err.Error()
	if strings.HasPrefix(msg, scraper.ClassNetwork+":") {
		return &scraper.Error{Provider: "server", Class: scraper.ClassNetwork, Msg: strings.TrimSpace(strings.TrimPrefix(msg, scraper.ClassNetwork+":"))}
	}
	return err
}

// Identify searches through the server: the same routing as scraper.Set
// (anime: Bangumi then TMDB tv/movie; otherwise TMDB), a few terms deep.
func (r Remote) Identify(_ context.Context, kind string, theatrical bool, q scraper.IdentifyQuery) ([]scraper.Candidate, error) {
	terms := append([]scraper.SearchTerm(nil), q.Terms...)
	sort.SliceStable(terms, func(i, j int) bool { return terms[i].Confidence > terms[j].Confidence })
	if len(terms) > 4 {
		terms = terms[:4]
	}
	type route struct{ provider, kind string }
	var routes []route
	switch kind {
	case "anime":
		tk := "tv"
		if theatrical {
			tk = "movie"
		}
		routes = []route{{"bgm", ""}, {"tmdb", tk}}
	case "tv":
		routes = []route{{"tmdb", "tv"}}
	default:
		routes = []route{{"tmdb", "movie"}}
	}
	pool := map[string]scraper.Candidate{}
	var firstErr error
	for _, rt := range routes {
		strong := false
		for _, t := range terms {
			cs, err := r.search(rt.provider, t.Name, rt.kind, q.Year)
			if err != nil {
				if fatal(err) {
					return nil, err
				}
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			for _, c := range cs {
				key := c.Provider + ":" + c.ID
				if old, ok := pool[key]; !ok || old.Score < c.Score {
					pool[key] = c
				}
				if c.Score >= scraper.StrongMatch {
					strong = true
				}
			}
			if strong {
				break
			}
		}
		if strong {
			break
		}
	}
	out := make([]scraper.Candidate, 0, len(pool))
	for _, c := range pool {
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Provider+out[i].ID < out[j].Provider+out[j].ID
	})
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
