package scraper

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// recorder saves the body of every 200 answer (bodies carry no key) under
// testdata/recorded, so the offline tests replay what the providers really
// send. Only with MEDIA_RECORD=1.
type recorder struct {
	next http.RoundTripper
	dir  string
	t    *testing.T
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func (r recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	if err != nil || resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return resp, err
	}
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(strings.NewReader(string(body)))
	if rerr == nil {
		// name from host and path only: the query string is where keys live
		name := unsafeName.ReplaceAllString(req.URL.Host+req.URL.Path, "_")
		if req.Method == http.MethodPost {
			name = "post_" + name
		}
		_ = os.MkdirAll(r.dir, 0o755)
		_ = os.WriteFile(filepath.Join(r.dir, name+".json"), body, 0o644)
	}
	return resp, nil
}

// findEnvFile looks for .secrets/media/media.env in the parents of the test
// directory, or at $MEDIA_ENV.
func findEnvFile() string {
	if p := os.Getenv("MEDIA_ENV"); p != "" {
		return p
	}
	dir, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, ".secrets", "media", "media.env")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

// TestLiveSmoke asks each real provider one question about Evangelion and
// asserts only that the answer has fields. It runs with MEDIA_LIVE=1 and
// reports ok/fail per provider; it never prints a value.
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("MEDIA_LIVE") != "1" {
		t.Skip("set MEDIA_LIVE=1 to call the real providers")
	}
	path := findEnvFile()
	if path == "" {
		t.Skip("no media.env found")
	}
	env, err := LoadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ConfigFromEnv(env)
	cfg.Timeout = 20 * time.Second
	if os.Getenv("MEDIA_RECORD") == "1" {
		hc, _ := cfg.httpClient()
		hc.Transport = recorder{next: hc.Transport, dir: filepath.Join("testdata", "recorded"), t: t}
		cfg.HTTPClient = hc
	}
	set, err := NewSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	check := func(provider string, f func() bool) {
		t.Run(provider, func(t *testing.T) {
			if f() {
				t.Logf("%s: ok", provider)
			} else {
				t.Errorf("%s: fail", provider)
			}
		})
	}

	var eva *ScrapeTarget
	check("tmdb", func() bool {
		if set.TMDB == nil {
			t.Log("no TMDB token in the env file")
			return false
		}
		tg := &ScrapeTarget{Kind: "tv"}
		tg.setID(ProviderTMDB, "tv/890")
		if err := set.TMDB.Fill(ctx, tg); err != nil {
			t.Logf("error class %s: %v", ClassOf(err), err)
			return false
		}
		eva = tg
		eps, err := set.TMDB.Season(ctx, "tv/890", 1)
		if err != nil {
			t.Logf("season: %v", err)
			return false
		}
		return tg.Title != "" && tg.Overview != "" && tg.PosterURL != "" && tg.ExternalIDs["imdb"] != "" && len(eps) > 0
	})
	check("bgm", func() bool {
		tg := &ScrapeTarget{}
		tg.setID(ProviderBGM, "265")
		if err := set.BGM.Fill(ctx, tg); err != nil {
			t.Logf("error class %s: %v", ClassOf(err), err)
			return false
		}
		eps, err := set.BGM.Episodes(ctx, "265")
		if err != nil {
			t.Logf("episodes: %v", err)
			return false
		}
		cs, err := set.BGM.Search(ctx, SearchQuery{Q: "新世纪福音战士"})
		if err != nil {
			t.Logf("search: %v", err)
			return false
		}
		return tg.Title != "" && tg.PosterURL != "" && len(eps) > 0 && len(cs) > 0
	})
	check("omdb", func() bool {
		if set.OMDb == nil {
			t.Log("no OMDb keys in the env file")
			return false
		}
		tg := &ScrapeTarget{}
		tg.setID("imdb", "tt0112159")
		if eva != nil && eva.ExternalIDs["imdb"] != "" {
			tg.setID("imdb", eva.ExternalIDs["imdb"])
		}
		if err := set.OMDb.Fill(ctx, tg); err != nil {
			t.Logf("error class %s: %v", ClassOf(err), err)
			return false
		}
		return tg.Ratings["imdb"] != nil
	})
	check("douban", func() bool {
		if set.Douban == nil {
			t.Log("no Douban key in the env file")
			return false
		}
		tg := &ScrapeTarget{}
		tg.setID("imdb", "tt0112159")
		if err := set.Douban.Fill(ctx, tg); err != nil {
			t.Logf("error class %s: %v", ClassOf(err), err)
			return false
		}
		return tg.ExternalIDs[ProviderDouban] != "" || tg.Ratings[ProviderDouban] != nil
	})
}
