package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/media/apply"
	"github.com/OpenListTeam/OpenList/v4/internal/media/identify"
	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
)

type fakeLister struct {
	tree  map[string][]identify.Entry
	calls []string
}

func (f *fakeLister) List(_ context.Context, p string) ([]identify.Entry, error) {
	f.calls = append(f.calls, p)
	es, ok := f.tree[p]
	if !ok {
		return nil, apply.ErrNotDir
	}
	return es, nil
}

func dir(name string) identify.Entry { return identify.Entry{Name: name, IsDir: true} }
func vid(name string) identify.Entry { return identify.Entry{Name: name, Size: 1 << 30} }
func sub(name string) identify.Entry { return identify.Entry{Name: name, Size: 40 << 10} }

type fakeItems []Item

func (f fakeItems) Items(context.Context, string) ([]Item, error) { return f, nil }

type fakeProber struct {
	tracks []Track
	err    error
	paths  []string
}

func (p *fakeProber) Probe(_ context.Context, path string) ([]Track, error) {
	p.paths = append(p.paths, path)
	return p.tracks, p.err
}

var testNow = time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

// providers fakes TMDB (tv/1: two seasons, 4 + 2 episodes, the 4th of season
// 1 not aired yet) and Bangumi (subject 265: 3 main episodes).
func providers(t *testing.T) *scraper.Set {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/3/tv/1":
			_, _ = w.Write([]byte(`{"id":1,"name":"Show","first_air_date":"2020-01-01","seasons":[{"season_number":0},{"season_number":1},{"season_number":2}]}`))
		case r.URL.Path == "/3/tv/1/season/1":
			_, _ = w.Write([]byte(`{"episodes":[{"episode_number":1,"air_date":"2020-01-01"},{"episode_number":2,"air_date":"2020-01-08"},{"episode_number":3,"air_date":"2020-01-15"},{"episode_number":4,"air_date":"2099-01-01"}]}`))
		case r.URL.Path == "/3/tv/1/season/2":
			_, _ = w.Write([]byte(`{"episodes":[{"episode_number":1,"air_date":"2021-01-01"},{"episode_number":2,"air_date":"2021-01-08"}]}`))
		case r.URL.Path == "/v0/episodes" && r.URL.Query().Get("subject_id") == "265":
			if r.URL.Query().Get("type") != "0" {
				t.Errorf("episodes asked for type %q, want 0", r.URL.Query().Get("type"))
			}
			_, _ = w.Write([]byte(`{"total":3,"data":[{"sort":1,"ep":1,"airdate":"1995-10-04"},{"sort":2,"ep":2,"airdate":"1995-10-11"},{"sort":3,"ep":3,"airdate":"1995-10-18"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	set, err := scraper.NewSet(scraper.Config{
		TMDBToken:  "eyJtest.token.x",
		BaseURLs:   map[string]string{"tmdb": srv.URL + "/3", "bgm": srv.URL},
		RatePerSec: map[string]float64{"tmdb": 1000, "bgm": 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func run(t *testing.T, ck *Checker, items ...Item) []Result {
	t.Helper()
	ck.Items = fakeItems(items)
	ck.Now = func() time.Time { return testNow }
	res, err := ck.Run(context.Background(), "/lib")
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestEpisodeNamingStyles(t *testing.T) {
	const vcb = "/lib/[VCB-Studio] Another [Ma10p_1080p]"
	const ani = "/lib/[ANi] Show - 1080P"
	const sxx = "/lib/Show.2020"
	var vcbFiles []identify.Entry
	for _, n := range []string{"01", "02", "03", "04", "06", "07"} {
		vcbFiles = append(vcbFiles, vid("[VCB-Studio] Another ["+n+"][Ma10p_1080p][x265_flac].mkv"))
	}
	vcbFiles = append(vcbFiles, dir("SPs"), dir("Scans"), dir("CDs"), vid("[VCB-Studio] Another [NCOP01][Ma10p_1080p].mkv"))
	lister := &fakeLister{tree: map[string][]identify.Entry{
		vcb:               vcbFiles,
		vcb + "/SPs":      {vid("[VCB-Studio] Another [SP01][Ma10p_1080p].mkv")},
		ani:               {vid("[ANi] Show - 01 [1080P][Baha][WEB-DL][AAC AVC][CHT].mp4"), vid("[ANi] Show - 03 [1080P][Baha][WEB-DL][AAC AVC][CHT].mp4")},
		sxx:               {dir("Season 1"), dir("Season 2"), dir("特典")},
		sxx + "/特典":       {vid("Making of.mkv")},
		sxx + "/Season 1": {vid("Show.S01E01.mkv"), vid("Show.S01E02.mkv"), vid("Show.S01E03.mkv"), vid("Show.S01E02.1080p.mkv")},
		sxx + "/Season 2": {vid("Show.S02E01.mkv")},
	}}
	ck := &Checker{Lister: lister, Providers: providers(t), Rules: Rules{
		vcb: {Expect: "S1: 1-8"},
	}}
	res := run(t, ck,
		Item{Path: vcb, Kind: "anime"},
		Item{Path: ani, Kind: "anime", ExternalIDs: map[string]string{"tmdb": "tv/1"}},
		Item{Path: sxx, Kind: "tv", ExternalIDs: map[string]string{"tmdb": "tv/1"}},
	)
	if len(res) != 3 {
		t.Fatalf("got %d results", len(res))
	}
	byPath := map[string]Result{}
	for _, r := range res {
		byPath[r.Item.Path] = r
	}

	v := byPath[vcb]
	if v.Features != 6 || v.Matched != 6 || len(v.Unparsed) != 0 {
		t.Errorf("vcb: features=%d matched=%d unparsed=%v", v.Features, v.Matched, v.Unparsed)
	}
	if !reflect.DeepEqual(v.Missing, map[int][]int{1: {5, 8}}) || v.ExpectSource != "rule" || v.Expected != 8 {
		t.Errorf("vcb: missing=%v source=%s expected=%d", v.Missing, v.ExpectSource, v.Expected)
	}
	for _, c := range lister.calls {
		if strings.HasSuffix(c, "/SPs") || strings.HasSuffix(c, "/Scans") || strings.HasSuffix(c, "/特典") {
			t.Errorf("listed an extras directory: %s", c)
		}
	}

	a := byPath[ani]
	if !reflect.DeepEqual(a.Missing, map[int][]int{1: {2}}) || a.Expected != 3 || a.Unaired != 1 || a.ExpectSource != "tmdb" {
		t.Errorf("ani: missing=%v expected=%d unaired=%d source=%s note=%q", a.Missing, a.Expected, a.Unaired, a.ExpectSource, a.ExpectNote)
	}

	s := byPath[sxx]
	// season 2 is present, so both seasons count: 3 + 2 aired, S02E02 missing
	if !reflect.DeepEqual(s.Missing, map[int][]int{2: {2}}) || s.Expected != 5 {
		t.Errorf("sxx: missing=%v expected=%d note=%q", s.Missing, s.Expected, s.ExpectNote)
	}
	if len(s.Dups) != 1 || !strings.HasPrefix(s.Dups[0], "S01E02") {
		t.Errorf("sxx: dups=%v", s.Dups)
	}
	if s.Features != 5 {
		t.Errorf("sxx: the 特典 file was counted: features=%d", s.Features)
	}
	if v.Flags&FlagMissing == 0 || a.Flags&FlagMissing == 0 || s.Flags&FlagMissing == 0 {
		t.Error("a missing episode must set FlagMissing")
	}
}

func TestOnlyPresentSeasonsAreChecked(t *testing.T) {
	const p = "/lib/Show"
	lister := &fakeLister{tree: map[string][]identify.Entry{p: {vid("Show.S01E01.mkv"), vid("Show.S01E02.mkv"), vid("Show.S01E03.mkv")}}}
	res := run(t, &Checker{Lister: lister, Providers: providers(t)}, Item{Path: p, Kind: "tv", ExternalIDs: map[string]string{"tmdb": "tv/1"}})
	if len(res[0].Missing) != 0 || res[0].Expected != 3 {
		t.Errorf("season 2 must not be expected when no file is season 2: missing=%v expected=%d", res[0].Missing, res[0].Expected)
	}
}

func TestBangumiEpisodes(t *testing.T) {
	const p = "/lib/EVA"
	lister := &fakeLister{tree: map[string][]identify.Entry{p: {vid("[Grp] EVA [01].mkv"), vid("[Grp] EVA [03].mkv")}}}
	res := run(t, &Checker{Lister: lister, Providers: providers(t)}, Item{Path: p, Kind: "anime", ExternalIDs: map[string]string{"bgm": "265"}})
	r := res[0]
	if r.ExpectSource != "bgm" || !reflect.DeepEqual(r.Missing, map[int][]int{1: {2}}) {
		t.Errorf("source=%s missing=%v", r.ExpectSource, r.Missing)
	}
}

func TestNoProviderInfersFromWhatIsThere(t *testing.T) {
	const p = "/lib/X"
	lister := &fakeLister{tree: map[string][]identify.Entry{p: {vid("X - 01.mkv"), vid("X - 02.mkv"), vid("X - 05.mkv")}}}
	r := run(t, &Checker{Lister: lister}, Item{Path: p, Kind: "tv"})[0]
	if r.ExpectSource != "inferred" || !reflect.DeepEqual(r.Missing, map[int][]int{1: {3, 4}}) || r.ExpectNote == "" {
		t.Errorf("source=%s missing=%v note=%q", r.ExpectSource, r.Missing, r.ExpectNote)
	}
}

func TestUnparsedNamesAndRuleRegexp(t *testing.T) {
	const p = "/lib/Odd"
	lister := &fakeLister{tree: map[string][]identify.Entry{p: {vid("Odd #1.mkv"), vid("Odd #2.mkv"), vid("Odd #3.mkv")}}}
	r := run(t, &Checker{Lister: lister}, Item{Path: p, Kind: "tv"})[0]
	if r.Flags&FlagParse == 0 || len(r.Unparsed) != 3 || r.Flags&FlagMissing != 0 {
		t.Fatalf("flags=%b unparsed=%v", r.Flags, r.Unparsed)
	}
	var buf bytes.Buffer
	if err := WriteReport(&buf, "/lib", []Result{r}, testNow); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "建议写 rules") || !strings.Contains(buf.String(), "`"+p+"`：3/3") {
		t.Errorf("the work isn't suggested for a rule:\n%s", buf.String())
	}

	r = run(t, &Checker{Lister: lister, Rules: Rules{p: {Episode: `#(?P<ep>\d+)`, Expect: "1-4"}}}, Item{Path: p, Kind: "tv"})[0]
	if r.Flags&FlagParse != 0 || r.Matched != 3 || !reflect.DeepEqual(r.Missing, map[int][]int{1: {4}}) {
		t.Errorf("with the rule: flags=%b matched=%d missing=%v", r.Flags, r.Matched, r.Missing)
	}
}

func TestRcloneFilterOverride(t *testing.T) {
	const p = "/lib/Show"
	tree := map[string][]identify.Entry{p: {vid("Show - 01.mkv"), vid("Show - 02.mkv"), vid("Show OP.mkv"), vid("Show - 03.mp4")}}
	r := run(t, &Checker{Lister: &fakeLister{tree: tree}}, Item{Path: p, Kind: "tv"})[0]
	if r.Features != 4 || len(r.Unparsed) != 1 {
		t.Fatalf("defaults: features=%d unparsed=%v", r.Features, r.Unparsed)
	}
	rules := Rules{p: {Files: []string{"- *OP*", "+ *.mkv"}}}
	r = run(t, &Checker{Lister: &fakeLister{tree: tree}, Rules: rules}, Item{Path: p, Kind: "tv"})[0]
	if r.Features != 2 || len(r.Unparsed) != 0 {
		t.Errorf("with the filter: features=%d unparsed=%v", r.Features, r.Unparsed)
	}
	if _, err := compileRule(Rule{Files: []string{"nonsense"}}); err == nil {
		t.Error("a malformed filter rule must be an error")
	}
}

func TestExternalSubtitleLanguages(t *testing.T) {
	const p = "/lib/Show"
	tree := map[string][]identify.Entry{p: {
		vid("Show - 01.mkv"), vid("Show - 02.mkv"),
		sub("Show - 01.chs.ass"), sub("Show - 01.cht.ass"), sub("Show - 01.jpn.srt"),
		sub("Show - 02.sc.ass"), sub("Show - 02.tc.ass"),
		dir("Subs"),
	}, p + "/Subs": {sub("[Grp] Show [02].eng.srt")}}
	run1 := func(rule Rule) Result {
		rules := Rules{p: rule}
		rules[p] = Rule{Expect: "1-2", Subs: rule.Subs}
		return run(t, &Checker{Lister: &fakeLister{tree: tree}, Rules: rules}, Item{Path: p, Kind: "tv"})[0]
	}
	r := run1(Rule{})
	if r.Flags != 0 || r.Subs.Covered != 2 || r.Subs.External != 6 {
		t.Fatalf("flags=%b subs=%+v", r.Flags, r.Subs)
	}
	want := map[string]int{"chs": 2, "cht": 2, "jpn": 1, "eng": 1}
	if !reflect.DeepEqual(r.Subs.Langs, want) {
		t.Errorf("langs=%v want %v", r.Subs.Langs, want)
	}
	var rule Rule
	rule.Subs.Langs = []string{"chs", "cht", "eng"}
	if r = run1(rule); r.Flags != 0 {
		t.Errorf("all wanted languages are there: flags=%b missing=%v", r.Flags, r.Subs.MissingLng)
	}
	rule.Subs.Langs = []string{"chs", "kor"}
	if r = run1(rule); r.Flags&FlagNoSubs == 0 || !reflect.DeepEqual(r.Subs.MissingLng, []string{"kor"}) {
		t.Errorf("a missing language must flag: flags=%b missing=%v", r.Flags, r.Subs.MissingLng)
	}
	rule = Rule{}
	rule.Subs.Mode = "none"
	if r = run1(rule); r.Flags != 0 || r.Subs.External != 0 {
		t.Errorf("mode none checks nothing: %+v", r.Subs)
	}
}

func TestSubtitleStatesAndProbe(t *testing.T) {
	const bare, hinted = "/lib/Bare", "/lib/[Grp] Hinted [内封简繁]"
	tree := map[string][]identify.Entry{
		bare:   {vid("Bare - 01.mkv"), vid("Bare - 02.mkv")},
		hinted: {vid("Hinted - 01.mkv"), vid("Hinted - 02.mkv")},
	}
	items := []Item{{Path: bare, Kind: "tv"}, {Path: hinted, Kind: "tv"}}
	rules := Rules{bare: {Expect: "1-2"}, hinted: {Expect: "1-2"}}

	res := run(t, &Checker{Lister: &fakeLister{tree: tree}, Rules: rules}, items...)
	if res[0].Flags != FlagNoSubs || res[1].Flags != FlagHint {
		t.Errorf("unprobed: bare=%b hinted=%b", res[0].Flags, res[1].Flags)
	}

	pr := &fakeProber{tracks: []Track{{Type: 1, Codec: "V_MPEGH/ISO/HEVC"}, {Type: TrackSubtitle, Codec: "S_TEXT/ASS", Lang: "chi"}}}
	res = run(t, &Checker{Lister: &fakeLister{tree: tree}, Rules: rules, Prober: pr}, items...)
	if res[0].Flags != 0 || res[1].Flags != 0 || !res[1].Subs.Probed || len(res[1].Subs.Tracks) != 1 {
		t.Errorf("probed with a subtitle track: bare=%b hinted=%b", res[0].Flags, res[1].Flags)
	}
	if len(pr.paths) != 2 || pr.paths[0] != bare+"/Bare - 01.mkv" {
		t.Errorf("one probe per work, on the first episode: %v", pr.paths)
	}

	pr = &fakeProber{tracks: []Track{{Type: 1}, {Type: 2, Lang: "jpn"}}}
	res = run(t, &Checker{Lister: &fakeLister{tree: tree}, Rules: rules, Prober: pr}, items...)
	if res[0].Flags != FlagNoSubs || res[1].Flags != FlagNoSubs {
		t.Errorf("probed without subtitle tracks: bare=%b hinted=%b", res[0].Flags, res[1].Flags)
	}

	pr = &fakeProber{err: errors.New("range not supported")}
	res = run(t, &Checker{Lister: &fakeLister{tree: tree}, Rules: rules, Prober: pr}, items...)
	if res[1].Flags != FlagHint || !strings.Contains(res[1].Subs.ProbeNote, "range not supported") {
		t.Errorf("a failed probe stays unprobed: flags=%b note=%q", res[1].Flags, res[1].Subs.ProbeNote)
	}

	// outside subtitles for every file settle it without a probe
	tree[bare] = append(tree[bare], sub("Bare - 01.ass"), sub("Bare - 02.ass"))
	pr = &fakeProber{}
	res = run(t, &Checker{Lister: &fakeLister{tree: tree}, Rules: rules, Prober: pr}, items[:1]...)
	if res[0].Flags != 0 || len(pr.paths) != 0 {
		t.Errorf("flags=%b probes=%v", res[0].Flags, pr.paths)
	}

	// only mkv can be probed
	tree[bare] = []identify.Entry{vid("Bare - 01.mp4")}
	pr = &fakeProber{}
	res = run(t, &Checker{Lister: &fakeLister{tree: tree}, Rules: Rules{bare: {Expect: "1"}}, Prober: pr}, items[:1]...)
	if len(pr.paths) != 0 || !strings.Contains(res[0].Subs.ProbeNote, "mkv") {
		t.Errorf("mp4 must not be probed: %v %q", pr.paths, res[0].Subs.ProbeNote)
	}
}

func TestMovies(t *testing.T) {
	tree := map[string][]identify.Entry{
		"/lib":                   {vid("Fight Club (1999).mkv"), sub("Fight Club (1999).chs.srt"), sub("Other.srt")},
		"/lib/Empty":             {dir("Extras")},
		"/lib/Empty/Extras":      {vid("Trailer.mkv")},
		"/lib/Two":               {vid("Two.CD1.mkv"), vid("Two.CD2.mkv")},
		"/lib/Alien (1979)":      {vid("Alien.1979.mkv"), dir("Subs")},
		"/lib/Alien (1979)/Subs": {sub("Alien.1979.eng.srt")},
	}
	lister := &fakeLister{tree: tree}
	res := run(t, &Checker{Lister: lister},
		Item{Path: "/lib/Fight Club (1999).mkv", Kind: "movie"},
		Item{Path: "/lib/Empty", Kind: "movie"},
		Item{Path: "/lib/Two", Kind: "movie"},
		Item{Path: "/lib/Alien (1979)", Kind: "movie"},
		Item{Path: "/lib/X", Kind: "ignore"},
		Item{Path: "/lib/Y", Kind: "collection"},
	)
	if len(res) != 4 {
		t.Fatalf("ignore and collection must be skipped: %d results", len(res))
	}
	by := map[string]Result{}
	for _, r := range res {
		by[r.Item.Path] = r
	}
	fc := by["/lib/Fight Club (1999).mkv"]
	if fc.Flags != 0 || fc.Subs.External != 1 || fc.Subs.Langs["chs"] != 1 || fc.Subs.Orphans != 0 {
		t.Errorf("single file movie: flags=%b subs=%+v", fc.Flags, fc.Subs)
	}
	if e := by["/lib/Empty"]; e.Flags&FlagMissing == 0 || e.Features != 0 {
		t.Errorf("a movie without a main video is missing: flags=%b features=%d", e.Flags, e.Features)
	}
	if two := by["/lib/Two"]; two.Flags&FlagMissing != 0 || two.ExpectNote == "" {
		t.Errorf("two videos: flags=%b note=%q", two.Flags, two.ExpectNote)
	}
	if al := by["/lib/Alien (1979)"]; al.Flags != 0 || al.Subs.Langs["eng"] != 1 {
		t.Errorf("subs dir for a movie: flags=%b subs=%+v", al.Flags, al.Subs)
	}
}

func TestListingFailureAndDepth(t *testing.T) {
	deep := map[string][]identify.Entry{
		"/lib/W":       {dir("a")},
		"/lib/W/a":     {dir("b"), vid("W - 01.mkv")},
		"/lib/W/a/b":   {dir("c"), vid("W - 02.mkv")},
		"/lib/W/a/b/c": {vid("W - 03.mkv")},
	}
	l := &fakeLister{tree: deep}
	r := run(t, &Checker{Lister: l, Rules: Rules{"/lib/W": {Expect: "1-3"}}}, Item{Path: "/lib/W", Kind: "tv"})[0]
	if r.Features != 2 || !reflect.DeepEqual(r.Missing, map[int][]int{1: {3}}) {
		t.Errorf("three levels only: features=%d missing=%v calls=%v", r.Features, r.Missing, l.calls)
	}
	if r.Calls != 3 {
		t.Errorf("calls=%d", r.Calls)
	}
}

func TestReportHasEveryPart(t *testing.T) {
	r := Result{
		Item: Item{Path: "/lib/A|B", Kind: "tv"}, Flags: FlagMissing | FlagNoSubs, Features: 3, Matched: 3,
		Expected: 5, ExpectSource: "tmdb", Missing: map[int][]int{1: {2, 4, 5}}, Calls: 2,
	}
	ok := Result{Item: Item{Path: "/lib/Ok", Kind: "movie"}, Features: 1, Matched: 1, Expected: 1, ExpectSource: "movie", Calls: 1}
	var buf bytes.Buffer
	if err := WriteReport(&buf, "/lib", []Result{r, ok}, testNow); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"⚠️缺集 🈚无字幕", "✅完整", "缺 S01：2, 4-5", "✅完整：1 / 2", "A\\|B", "列目录请求：3"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestLoadRules(t *testing.T) {
	f := filepath.Join(t.TempDir(), "rules.yaml")
	_ = os.WriteFile(f, []byte(`"/115/x/[VCB-Studio] Another [Ma10p_1080p]/":
  files: ["- SPs/**", "+ *.mkv"]
  episode: '\[(?P<ep>\d{2})\]'
  expect: "S1: 1-12"
  subs: { langs: [chs, cht], mode: any }
`), 0o600)
	rules, err := LoadRules(f)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := rules["/115/x/[VCB-Studio] Another [Ma10p_1080p]"]
	if !ok {
		t.Fatalf("the trailing slash must not matter: %v", rules)
	}
	c, err := compileRule(r)
	if err != nil {
		t.Fatal(err)
	}
	if c.filter == nil || c.epRe == nil || len(c.expect[1]) != 12 || c.mode != "any" || len(c.langs) != 2 {
		t.Errorf("compiled: %+v", c)
	}
	if c.filter.IncludeRemote("SPs/a.mkv") || !c.filter.IncludeRemote("a.mkv") || c.filter.IncludeRemote("a.txt") {
		t.Error("the rclone filter doesn't behave")
	}
	for _, bad := range []Rule{{Episode: `(?P<n>\d+)`}, {Episode: `(`}, {Expect: "S1: x"}, {Expect: "5-2"}} {
		if _, err := compileRule(bad); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
	got, _ := ParseExpect("S1: 1-3, 5; S2: 1-2")
	if len(got[1]) != 4 || len(got[2]) != 2 || !got[1][5] {
		t.Errorf("ParseExpect: %v", got)
	}
	if m, err := ParseExpect("auto"); m != nil || err != nil {
		t.Errorf("auto: %v %v", m, err)
	}
}

func TestSubLangs(t *testing.T) {
	for _, c := range []struct {
		sub, stem string
		want      []string
	}{
		{"Show - 01.chs.ass", "Show - 01", []string{"chs"}},
		{"Show - 01.zh-Hans.srt", "Show - 01", []string{"und"}},
		{"Show - 01.简日.ass", "Show - 01", []string{"chs", "jpn"}},
		{"Show - 01.繁体.ass", "Show - 01", []string{"cht"}},
		{"Show - 01.CHT&JPN.ass", "Show - 01", []string{"cht", "jpn"}},
		{"Show - 01.ass", "Show - 01", []string{"und"}},
		{"English Teacher 01.ass", "English Teacher 01", []string{"und"}},
		{"[Grp] Show [01][SC].ass", "", []string{"chs"}},
	} {
		if got := subLangs(c.sub, c.stem); !reflect.DeepEqual(got, c.want) {
			t.Errorf("subLangs(%q, %q) = %v, want %v", c.sub, c.stem, got, c.want)
		}
	}
	if !reflect.DeepEqual(trackLang("zh-Hant"), []string{"cht"}) || !reflect.DeepEqual(trackLang("chi"), []string{"chs", "cht"}) {
		t.Error("trackLang")
	}
	_ = json.Marshal
}
