package scraper

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

func secs(n int) time.Duration { return time.Duration(n) * time.Second }

// Names of the variables in a media.env file.
const (
	EnvTMDBToken  = "TMDB_READ_TOKEN"
	EnvOMDbKeys   = "OMDB_KEYS"
	EnvDoubanKey  = "DOUBAN_APIKEY"
	EnvLanguage   = "MEDIA_LANGUAGE"
	EnvHTTPProxy  = "MEDIA_HTTP_PROXY"
	EnvTimeoutSec = "MEDIA_SCRAPER_TIMEOUT"
)

// LoadEnvFile reads a KEY=VALUE file. It never prints or logs the values, and
// its errors carry only the path and line number.
func LoadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open env file: %w", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	return out, nil
}

// ConfigFromEnv builds a Config from the variables of an env file.
func ConfigFromEnv(env map[string]string) Config {
	cfg := Config{
		TMDBToken:    env[EnvTMDBToken],
		OMDbKeys:     SplitKeys(env[EnvOMDbKeys]),
		DoubanAPIKey: env[EnvDoubanKey],
		Language:     env[EnvLanguage],
		Proxy:        env[EnvHTTPProxy],
	}
	if s := env[EnvTimeoutSec]; s != "" {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
			cfg.Timeout = secs(n)
		}
	}
	return cfg
}
