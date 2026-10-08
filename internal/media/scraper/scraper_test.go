package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// Fake keys: nothing in this file is a real credential.
const (
	fakeBearer = "eyJhbGciOiJIUzI1NiJ9.fake-payload.fake-signature"
	fakeV3Key  = "0123456789abcdef0123456789abcdef"
	fakeOMDb1  = "omdbfake1"
	fakeOMDb2  = "omdbfake2"
	fakeOMDb3  = "omdbfake3"
	fakeDouban = "doubanfakekey99"
)

// fixture loads a recorded provider answer. They were recorded from the real
// providers for Evangelion (the request key is not in a body) and trimmed.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "recorded", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func fastConfig(base map[string]string) Config {
	return Config{
		BaseURLs:   base,
		RatePerSec: map[string]float64{"tmdb": 1000, "bgm": 1000, "omdb": 1000, "douban": 1000},
		RetryDelay: time.Millisecond,
		Timeout:    5 * time.Second,
	}
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func tmdbFake(t *testing.T, seen *[]*http.Request) *httptest.Server {
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = append(*seen, r.Clone(context.Background()))
		}
		switch {
		case r.URL.Path == "/3/tv/890":
			writeJSON(w, fixture(t, "api_themoviedb_org_3_tv_890.json"))
		case r.URL.Path == "/3/tv/890/season/1":
			writeJSON(w, fixture(t, "api_themoviedb_org_3_tv_890_season_1.json"))
		case strings.HasPrefix(r.URL.Path, "/3/search/"):
			writeJSON(w, []byte(`{"results":[
			 {"id":890,"name":"新世纪福音战士","first_air_date":"1995-10-04","vote_average":8.5,"poster_path":"/p.jpg"},
			 {"id":1,"name":"Something Else","first_air_date":"2001-01-01","vote_average":3}]}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func TestTMDBAuthBearerAndAPIKey(t *testing.T) {
	var seen []*http.Request
	srv := tmdbFake(t, &seen)

	// a v4 token is sent as a Bearer header and never as a query parameter
	cfg := fastConfig(map[string]string{"tmdb": srv.URL + "/3"})
	cfg.TMDBToken = fakeBearer
	set, _ := NewSet(cfg)
	if _, err := set.TMDB.Search(context.Background(), SearchQuery{Q: "新世纪福音战士", Kind: "tv"}); err != nil {
		t.Fatal(err)
	}
	r := seen[0]
	if got := r.Header.Get("Authorization"); got != "Bearer "+fakeBearer {
		t.Fatalf("Authorization = %q", got)
	}
	if r.URL.Query().Has("api_key") {
		t.Fatal("bearer token also leaked into the query string")
	}

	// a v3 key goes in api_key, with no Authorization header
	seen = nil
	cfg.TMDBToken = fakeV3Key
	set, _ = NewSet(cfg)
	if _, err := set.TMDB.Search(context.Background(), SearchQuery{Q: "新世纪福音战士", Kind: "tv"}); err != nil {
		t.Fatal(err)
	}
	r = seen[0]
	if r.URL.Query().Get("api_key") != fakeV3Key {
		t.Fatalf("api_key = %q", r.URL.Query().Get("api_key"))
	}
	if r.Header.Get("Authorization") != "" {
		t.Fatal("v3 key sent as a header too")
	}
	if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "openlist-media/") {
		t.Fatalf("User-Agent = %q", ua)
	}
}

func TestTMDBFillFromRecordedAnswer(t *testing.T) {
	var seen []*http.Request
	srv := tmdbFake(t, &seen)
	cfg := fastConfig(map[string]string{"tmdb": srv.URL + "/3"})
	cfg.TMDBToken = fakeBearer
	cfg.Language = "zh-CN"
	set, _ := NewSet(cfg)

	tg := &ScrapeTarget{Kind: "tv"}
	tg.setID(ProviderTMDB, "tv/890")
	if err := set.TMDB.Fill(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	q := seen[0].URL.Query()
	if q.Get("language") != "zh-CN" || q.Get("append_to_response") != "external_ids" {
		t.Fatalf("detail query = %v", q)
	}
	if tg.ExternalIDs["imdb"] != "tt0112159" || tg.ExternalIDs["tmdb"] != "tv/890" {
		t.Fatalf("ids = %v", tg.ExternalIDs)
	}
	if tg.Year != 1995 || tg.Title == "" || tg.Overview == "" || len(tg.Genres) == 0 {
		t.Fatalf("fields not filled: %+v", tg)
	}
	if !strings.HasPrefix(tg.PosterURL, "https://image.tmdb.org/t/p/w500/") || !strings.HasPrefix(tg.BackdropURL, "https://image.tmdb.org/t/p/w1280/") {
		t.Fatalf("image urls: %q %q", tg.PosterURL, tg.BackdropURL)
	}
	if v, _ := tg.Ratings["tmdb"].(float64); v <= 0 {
		t.Fatalf("rating = %v", tg.Ratings)
	}
	if len(tg.Seasons) == 0 || tg.Seasons[0] != 1 {
		t.Fatalf("seasons = %v", tg.Seasons)
	}

	eps, err := set.TMDB.Season(context.Background(), "tv/890", 1)
	if err != nil || len(eps) != 3 || eps[0].Episode != 1 || eps[0].Season != 1 || eps[0].Title == "" {
		t.Fatalf("season: %+v %v", eps, err)
	}
}

func TestTMDBIdentifyStopsAtFirstStrongHit(t *testing.T) {
	var seen []*http.Request
	srv := tmdbFake(t, &seen)
	cfg := fastConfig(map[string]string{"tmdb": srv.URL + "/3"})
	cfg.TMDBToken = fakeBearer
	set, _ := NewSet(cfg)

	p := ParseName("新世纪福音战士 (1995)")
	cs, err := set.TMDB.Identify(context.Background(), IdentifyQuery{Terms: SearchTerms(p), Kind: "tv", Year: 1995})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) == 0 || cs[0].ID != "tv/890" || cs[0].Score < StrongMatch {
		t.Fatalf("candidates = %+v", cs)
	}
	if len(seen) != 1 {
		t.Fatalf("%d searches for a name that matches at once", len(seen))
	}
	if cs[0].Year != 1995 || !strings.HasPrefix(cs[0].PosterURL, "https://image.tmdb.org/t/p/w342/") {
		t.Fatalf("candidate fields: %+v", cs[0])
	}
}

func TestTMDBMovieVersusTVEndpoint(t *testing.T) {
	var paths []string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		writeJSON(w, []byte(`{"results":[]}`))
	})
	cfg := fastConfig(map[string]string{"tmdb": srv.URL + "/3"})
	cfg.TMDBToken = fakeBearer
	set, _ := NewSet(cfg)
	_, _ = set.TMDB.Identify(context.Background(), IdentifyQuery{Terms: SearchTerms(ParseName("盗梦空间")), Kind: "movie"})
	if len(paths) == 0 || paths[0] != "/3/search/movie" {
		t.Fatalf("movie went to %v", paths)
	}
	paths = nil
	_, _ = set.TMDB.Identify(context.Background(), IdentifyQuery{Terms: SearchTerms(ParseName("盗梦空间")), Kind: "tv"})
	if len(paths) == 0 || paths[0] != "/3/search/tv" {
		t.Fatalf("tv went to %v", paths)
	}
	if len(paths) > tmdbMaxAttempts {
		t.Fatalf("%d requests for one name", len(paths))
	}
}

func TestBangumiUserAgentAndSearch(t *testing.T) {
	var ua, ct, method string
	var body map[string]any
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		ua, ct, method = r.Header.Get("User-Agent"), r.Header.Get("Content-Type"), r.Method
		switch {
		case r.URL.Path == "/v0/search/subjects":
			_ = json.NewDecoder(r.Body).Decode(&body)
			writeJSON(w, fixture(t, "post_api_bgm_tv_v0_search_subjects.json"))
		case r.URL.Path == "/v0/subjects/265":
			writeJSON(w, fixture(t, "api_bgm_tv_v0_subjects_265.json"))
		case r.URL.Path == "/v0/episodes":
			writeJSON(w, fixture(t, "api_bgm_tv_v0_episodes.json"))
		default:
			http.NotFound(w, r)
		}
	})
	Version = "9.9.9"
	t.Cleanup(func() { Version = "dev" })
	set, _ := NewSet(fastConfig(map[string]string{"bgm": srv.URL}))

	cs, err := set.BGM.Search(context.Background(), SearchQuery{Q: "新世纪福音战士"})
	if err != nil || len(cs) == 0 {
		t.Fatalf("search: %v %v", cs, err)
	}
	if ua != "openlist-media/9.9.9" {
		t.Fatalf("User-Agent = %q, want openlist-media/9.9.9", ua)
	}
	if method != http.MethodPost || !strings.HasPrefix(ct, "application/json") || body["keyword"] != "新世纪福音战士" {
		t.Fatalf("search request: %s %s %v", method, ct, body)
	}
	if cs[0].Provider != "bgm" || cs[0].ID == "" || cs[0].Kind != "anime" {
		t.Fatalf("candidate: %+v", cs[0])
	}

	tg := &ScrapeTarget{}
	tg.setID(ProviderBGM, "265")
	if err := set.BGM.Fill(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	if tg.Title == "" || tg.PosterURL == "" || tg.Year == 0 || tg.Kind != "anime" {
		t.Fatalf("bgm fill: %+v", tg)
	}
	if v, _ := tg.Ratings["bgm"].(float64); v <= 0 {
		t.Fatalf("bgm rating: %v", tg.Ratings)
	}
	for _, g := range tg.Genres {
		if bgmYearTagRe.MatchString(g) {
			t.Fatalf("year tag kept as genre: %q", g)
		}
	}
	eps, err := set.BGM.Episodes(context.Background(), "265")
	if err != nil || len(eps) != 5 || eps[0].Season != 1 || eps[0].Episode != 1 {
		t.Fatalf("episodes: %+v %v", eps, err)
	}
}

func TestOMDbPicksAmongKeysAndFailsOver(t *testing.T) {
	var mu sync.Mutex
	used := map[string]int{}
	rejected := map[string]bool{fakeOMDb3: true}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("apikey")
		mu.Lock()
		used[key]++
		mu.Unlock()
		if rejected[key] {
			writeJSON(w, []byte(`{"Response":"False","Error":"Invalid API key!"}`))
			return
		}
		writeJSON(w, []byte(`{"Response":"True","imdbRating":"8.5","Ratings":[{"Source":"Rotten Tomatoes","Value":"94%"}]}`))
	})
	next := 0
	cfg := fastConfig(map[string]string{"omdb": srv.URL})
	cfg.OMDbKeys = []string{fakeOMDb1, fakeOMDb2, fakeOMDb3}
	cfg.Intn = func(n int) int { v := next % n; next++; return v }
	set, _ := NewSet(cfg)

	for i := 0; i < 3; i++ {
		tg := &ScrapeTarget{}
		tg.setID("imdb", "tt0112159")
		if err := set.OMDb.Fill(context.Background(), tg); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
		if tg.Ratings["imdb"] != 8.5 || tg.Ratings["rt"] != "94%" {
			t.Fatalf("ratings = %v", tg.Ratings)
		}
	}
	// the random pick landed on each key once; the rejected one fell over to a good one
	if used[fakeOMDb1] == 0 || used[fakeOMDb2] == 0 || used[fakeOMDb3] != 1 {
		t.Fatalf("key use = %v", used)
	}

	// every key rejected -> an auth-class error, no panic
	rejected[fakeOMDb1], rejected[fakeOMDb2] = true, true
	tg := &ScrapeTarget{}
	tg.setID("imdb", "tt1")
	err := set.OMDb.Fill(context.Background(), tg)
	if ClassOf(err) != ClassAuth {
		t.Fatalf("err = %v (class %q)", err, ClassOf(err))
	}
}

func TestDoubanPostsKeyInFormNotURL(t *testing.T) {
	var method, ct, rawQuery, path string
	var form url.Values
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		method, ct, rawQuery, path = r.Method, r.Header.Get("Content-Type"), r.URL.RawQuery, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
		writeJSON(w, fixture(t, "post_api_douban_com_v2_movie_imdb_tt0112159.json"))
	})
	cfg := fastConfig(map[string]string{"douban": srv.URL})
	cfg.DoubanAPIKey = fakeDouban
	set, _ := NewSet(cfg)

	tg := &ScrapeTarget{}
	tg.setID("imdb", "tt0112159")
	if err := set.Douban.Fill(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/v2/movie/imdb/tt0112159" {
		t.Fatalf("request: %s %s", method, path)
	}
	if !strings.HasPrefix(ct, "application/x-www-form-urlencoded") || form.Get("apikey") != fakeDouban {
		t.Fatalf("form: ct=%q form=%v", ct, form)
	}
	if rawQuery != "" {
		t.Fatalf("key leaked into the URL: %q", rawQuery)
	}
	if tg.ExternalIDs["douban"] != "1457573" || tg.Ratings["douban"] != 9.3 {
		t.Fatalf("douban fields: %v %v", tg.ExternalIDs, tg.Ratings)
	}
}

func TestSetFetchEnrichesWithRatings(t *testing.T) {
	tm := tmdbFake(t, nil)
	om := serve(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, fixture(t, "www_omdbapi_com_.json")) })
	db := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, fixture(t, "post_api_douban_com_v2_movie_imdb_tt0112159.json"))
	})
	cfg := fastConfig(map[string]string{"tmdb": tm.URL + "/3", "omdb": om.URL, "douban": db.URL})
	cfg.TMDBToken, cfg.OMDbKeys, cfg.DoubanAPIKey = fakeBearer, []string{fakeOMDb1}, fakeDouban
	set, _ := NewSet(cfg)

	tg, warns, err := set.Fetch(context.Background(), "tmdb", "tv/890", "tv")
	if err != nil || len(warns) != 0 {
		t.Fatalf("fetch: %v %v", warns, err)
	}
	for _, k := range []string{"tmdb", "imdb", "douban"} {
		if tg.Ratings[k] == nil {
			t.Errorf("rating %q missing: %v", k, tg.Ratings)
		}
	}
	if tg.ExternalIDs["douban"] == "" {
		t.Errorf("douban id missing: %v", tg.ExternalIDs)
	}
}

func TestSetFetchEnrichmentFailureIsOnlyAWarning(t *testing.T) {
	tm := tmdbFake(t, nil)
	om := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 401) })
	cfg := fastConfig(map[string]string{"tmdb": tm.URL + "/3", "omdb": om.URL})
	cfg.TMDBToken, cfg.OMDbKeys = fakeBearer, []string{fakeOMDb1}
	set, _ := NewSet(cfg)
	tg, warns, err := set.Fetch(context.Background(), "tmdb", "tv/890", "tv")
	if err != nil || tg == nil {
		t.Fatalf("a failing OMDb must not fail the fetch: %v", err)
	}
	if len(warns) != 1 || strings.Contains(warns[0], fakeOMDb1) {
		t.Fatalf("warnings = %v", warns)
	}
}

func TestAnimeRoutesToBangumiFirst(t *testing.T) {
	var tmdbHits int
	tm := serve(t, func(w http.ResponseWriter, r *http.Request) { tmdbHits++; writeJSON(w, []byte(`{"results":[]}`)) })
	bg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []byte(`{"data":[{"id":265,"name":"新世紀エヴァンゲリオン","name_cn":"新世纪福音战士","date":"1995-10-04","score":8.9,"image":"https://lain.bgm.tv/x.jpg"}]}`))
	})
	cfg := fastConfig(map[string]string{"tmdb": tm.URL + "/3", "bgm": bg.URL})
	cfg.TMDBToken = fakeBearer
	set, _ := NewSet(cfg)
	cs, err := set.Identify(context.Background(), "anime", false, IdentifyQuery{Terms: SearchTerms(ParseName("新世纪福音战士")), Year: 1995})
	if err != nil || len(cs) == 0 || cs[0].Provider != "bgm" || cs[0].ID != "265" {
		t.Fatalf("candidates = %+v, %v", cs, err)
	}
	if tmdbHits != 0 {
		t.Fatalf("TMDB queried %d times although Bangumi matched", tmdbHits)
	}
}

func TestErrorClasses(t *testing.T) {
	status := 200
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			http.Error(w, "x", status)
			return
		}
		writeJSON(w, []byte(`{"results":[]}`))
	})
	cfg := fastConfig(map[string]string{"tmdb": srv.URL + "/3"})
	cfg.TMDBToken = fakeBearer
	set, _ := NewSet(cfg)
	q := SearchQuery{Q: "x", Kind: "tv"}
	for code, want := range map[int]string{401: ClassAuth, 404: ClassNotFound, 429: ClassRate, 503: ClassOther} {
		status = code
		_, err := set.TMDB.Search(context.Background(), q)
		if ClassOf(err) != want {
			t.Errorf("HTTP %d -> %q, want %q (%v)", code, ClassOf(err), want, err)
		}
	}

	// a server nobody listens on is a network error
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	cfg = fastConfig(map[string]string{"tmdb": deadURL + "/3", "bgm": deadURL})
	cfg.TMDBToken = fakeBearer
	set, _ = NewSet(cfg)
	_, err := set.TMDB.Search(context.Background(), q)
	if ClassOf(err) != ClassNetwork {
		t.Errorf("dead server -> %q (%v)", ClassOf(err), err)
	}
	// ...and Identify gives up at once instead of trying 14 searches
	start := time.Now()
	_, err = set.TMDB.Identify(context.Background(), IdentifyQuery{Terms: SearchTerms(ParseName("盗梦空间 2010")), Kind: "movie"})
	if ClassOf(err) != ClassNetwork || time.Since(start) > 3*time.Second {
		t.Errorf("identify on a dead network: %v after %v", err, time.Since(start))
	}
	if _, err := set.BGM.Search(context.Background(), q); ClassOf(err) != ClassNetwork {
		t.Errorf("bgm dead server -> %q", ClassOf(err))
	}
}

func TestSecretsNeverReachLogsOrErrors(t *testing.T) {
	var logs bytes.Buffer
	old, oldLvl := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&logs)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() { log.SetOutput(old); log.SetLevel(oldLvl) })

	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key "+r.URL.Query().Get("api_key"), 401)
	})
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	var allErrs []string
	for _, base := range []string{srv.URL, deadURL} {
		for _, token := range []string{fakeV3Key, fakeBearer} {
			cfg := fastConfig(map[string]string{"tmdb": base + "/3"})
			cfg.TMDBToken = token
			set, _ := NewSet(cfg)
			_, err := set.TMDB.Search(context.Background(), SearchQuery{Q: "x", Kind: "tv"})
			if err == nil {
				t.Fatal("expected an error")
			}
			allErrs = append(allErrs, err.Error())
		}
	}
	out := logs.String() + strings.Join(allErrs, "\n")
	for _, secret := range []string{fakeV3Key, fakeBearer, "fake-payload"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q found in logs/errors:\n%s", secret, out)
		}
	}
	if !strings.Contains(logs.String(), "[media/tmdb]") {
		t.Fatal("test did not exercise the logging path")
	}
}

func TestMaskSecrets(t *testing.T) {
	cases := []struct{ in, notWant string }{
		{"GET https://api.themoviedb.org/3/search/tv?api_key=SECRETKEY123&query=x", "SECRETKEY123"},
		{"https://www.omdbapi.com/?apikey=ABCDEF1234&i=tt1", "ABCDEF1234"},
		{"Authorization: Bearer eyJabc.def.ghi", "eyJabc.def.ghi"},
		{"token=zzzzzz999&x=1", "zzzzzz999"},
	}
	for _, c := range cases {
		got := MaskSecrets(c.in)
		if strings.Contains(got, c.notWant) || !strings.Contains(got, "***") {
			t.Errorf("MaskSecrets(%q) = %q", c.in, got)
		}
	}
	if got := MaskSecrets("plain text with literal hunter22secret in it", "hunter22secret"); strings.Contains(got, "hunter22secret") {
		t.Errorf("literal secret survived: %q", got)
	}
	if got := MaskSecrets("GET /search?query=x&page=1"); got != "GET /search?query=x&page=1" {
		t.Errorf("innocent text changed: %q", got)
	}
}

func TestEnvFileLoadsWithoutEchoingValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "media.env")
	content := "# comment\nTMDB_READ_TOKEN=\"" + fakeBearer + "\"\nOMDB_KEYS=" + fakeOMDb1 + ", " + fakeOMDb2 + "\nexport DOUBAN_APIKEY=" + fakeDouban + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := LoadEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ConfigFromEnv(env)
	if cfg.TMDBToken != fakeBearer || len(cfg.OMDbKeys) != 2 || cfg.OMDbKeys[1] != fakeOMDb2 || cfg.DoubanAPIKey != fakeDouban {
		t.Fatalf("config = %+v", cfg)
	}
	bad := filepath.Join(t.TempDir(), "bad.env")
	_ = os.WriteFile(bad, []byte("this line has secret-looking-value but no equals\n"), 0o600)
	if _, err := LoadEnvFile(bad); err == nil || strings.Contains(err.Error(), "secret-looking-value") {
		t.Fatalf("error echoes the file content: %v", err)
	}
}

func TestPortedHelpers(t *testing.T) {
	// REFACTOR_PLAN 9.7 samples for the ported parser
	cases := []struct{ in, en, cn, year string }{
		{"Inception.2010.盗梦空间.双语字幕.HR-HDTV.AC3.1024X576.X264-CMCT.mkv", "Inception", "盗梦空间", "2010"},
		{"Iron.Man.3.2013.钢铁侠3.国英音轨.双语字幕.HR-HDTV.AC3.x264-CMCT.mkv", "Iron Man 3", "钢铁侠3", "2013"},
		{"19 [复仇者联盟3：无限战争]Avengers.Infinity.War.2018.2160p.BluRay.mkv", "Avengers Infinity War", "复仇者联盟3：无限战争", "2018"},
		// dev-media keeps 3-digit leading numbers ("300.斯巴达勇士"), so the 169
		// serial stays as a stray English fragment; the Chinese title is right.
		{"169.谍影重重3.mkv", "169", "谍影重重3", ""},
		{"1.漫威短片.47号物品.mkv", "", "漫威短片", ""},
		{"30.Days.of.Night.2007.mkv", "30 Days of Night", "", "2007"},
	}
	for _, c := range cases {
		p := ParseName(c.in)
		if p.English != c.en || p.Chinese != c.cn || p.Year != c.year {
			t.Errorf("ParseName(%q) = %q / %q / %q, want %q / %q / %q", c.in, p.English, p.Chinese, p.Year, c.en, c.cn, c.year)
		}
	}
	if p := ParseName("1.漫威短片.47号物品.mkv"); !contains(p.Extra, "47号物品") {
		t.Errorf("47号物品 missing from the extra candidates: %v", p.Extra)
	}
	if PickEndpoint("Show.S01E02.mkv") != "tv" || PickEndpoint("第3季") != "tv" || PickEndpoint("Inception.2010.mkv") != "movie" {
		t.Error("PickEndpoint")
	}
	// scoreMatch: year distance tiers and the neutral no-year score
	hit := func(y string) tmdbHit { return tmdbHit{Title: "Inception", ReleaseDate: y + "-07-16", VoteAverage: 8} }
	s0, s1, s2, s5 := scoreMatch("Inception", "2010", hit("2010")), scoreMatch("Inception", "2010", hit("2011")), scoreMatch("Inception", "2010", hit("2012")), scoreMatch("Inception", "2010", hit("2015"))
	if !(s0 > s1 && s1 > s2 && s2 > s5) {
		t.Errorf("year tiers not decreasing: %v %v %v %v", s0, s1, s2, s5)
	}
	if n := scoreMatch("Inception", "", hit("2010")); n <= s5 || n >= s0 {
		t.Errorf("no-year score %v should sit between %v and %v", n, s5, s0)
	}
	if got := BuildTitleCandidates("钢铁侠3"); !contains(got, "钢铁侠") || !contains(got, "钢铁侠三") {
		t.Errorf("candidates for 钢铁侠3 = %v", got)
	}
	if got := BuildTitleCandidates("复仇者联盟3：无限战争"); !contains(got, "复仇者联盟3") || !contains(got, "无限战争") {
		t.Errorf("subtitle split = %v", got)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
