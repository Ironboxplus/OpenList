package identify

import (
	"fmt"
	"testing"
)

const gb = int64(1) << 30

func vids(names ...string) []Entry {
	var es []Entry
	for _, n := range names {
		es = append(es, Entry{Name: n, Size: gb})
	}
	return es
}

func dirs(names ...string) []Entry {
	var es []Entry
	for _, n := range names {
		es = append(es, Entry{Name: n, IsDir: true})
	}
	return es
}

// eps makes n episode files from a printf pattern taking the episode number.
func eps(pattern string, n int) []Entry {
	var es []Entry
	for i := 1; i <= n; i++ {
		es = append(es, Entry{Name: fmt.Sprintf(pattern, i), Size: gb})
	}
	return es
}

func one(name string) []Entry { return []Entry{{Name: name, Size: 8 * gb}} }

func cat(parts ...[]Entry) []Entry {
	var all []Entry
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}

// The names come from the user's libraries (the /115 download folder and the
// 115open MOV tree) and from the sample list in REFACTOR_PLAN 9.7; the
// expected values were annotated by hand before the rules were run. The
// listings are typical for each name, not captured.
var samples = []struct {
	name       string
	isDir      bool
	entries    []Entry
	kind       string
	title      string
	year       int
	season     int
	theatrical bool
}{
	// --- from PLAN_MEDIA section 7 ---
	{name: "【高清剧集网发布 www.BTHDTV.com】鬼谜东宫[全8集][国语配音+中文字幕].Ghost.Mystery.S01.2025.2160p.WEB-DL.H265.AAC-XXX", isDir: true,
		entries: eps("Ghost.Mystery.S01E%02d.2160p.mkv", 8), kind: "tv", title: "鬼谜东宫", season: 1},
	{name: "[VCB-Studio] Steins;Gate 0 [Ma10p_1080p]", isDir: true,
		entries: eps("[VCB-Studio] Steins;Gate 0 [%02d][Ma10p_1080p][x265_flac].mkv", 23), kind: "anime", title: "Steins;Gate 0"},
	{name: "Fight Club (1999) [1080p BluRay x265 10bit].mkv", kind: "movie", title: "Fight Club", year: 1999},
	{name: "三国演义94", isDir: true, entries: dirs("第1部（群雄逐鹿）", "第2部（赤壁鏖战）", "第3部（三足鼎立）", "第4部（南征北战）", "第5部（三分归一）"),
		kind: "tv", title: "三国演义", year: 1994},
	{name: "[DBD-Raws][4K_HDR][超时空要塞 可曾记得爱][2160P][BDRip][HEVC-10bit][FLAC][MKV]", isDir: true,
		entries: one("[DBD-Raws][4K_HDR][超时空要塞 可曾记得爱][2160P][BDRip][HEVC-10bit][FLAC].mkv"), kind: "anime", title: "超时空要塞 可曾记得爱", theatrical: true},

	// --- REFACTOR_PLAN 9.7 ---
	{name: "Inception.2010.盗梦空间.双语字幕.HR-HDTV.AC3.1024X576.X264-CMCT.mkv", kind: "movie", title: "盗梦空间", year: 2010},
	{name: "Iron.Man.3.2013.钢铁侠3.国英音轨.双语字幕.HR-HDTV.AC3.x264-CMCT.mkv", kind: "movie", title: "钢铁侠3", year: 2013},
	{name: "19 [复仇者联盟3：无限战争]Avengers.Infinity.War.2018.2160p.BluRay.mkv", kind: "movie", title: "复仇者联盟3：无限战争", year: 2018},
	{name: "169.谍影重重3", isDir: true, entries: one("The.Bourne.Ultimatum.2007.mkv"), kind: "movie", title: "谍影重重3"},
	{name: "1.漫威短片.47号物品.mkv", kind: "movie", title: "漫威短片"},
	{name: "300.斯巴达勇士", isDir: true, entries: one("300.2006.1080p.mkv"), kind: "movie", title: "斯巴达勇士"},
	{name: "30.Days.of.Night.2007.1080p.BluRay.x264.mkv", kind: "movie", title: "30 Days of Night", year: 2007},

	// --- movies, one main file ---
	{name: "Lock, Stock and Two Smoking Barrels (1998).mkv", kind: "movie", title: "Lock, Stock and Two Smoking Barrels", year: 1998},
	{name: "盗梦空间", isDir: true, entries: cat(one("Inception.2010.mkv"), []Entry{{Name: "sample.mkv", Size: 40 << 20}, {Name: "poster.jpg", Size: 1 << 20}}), kind: "movie", title: "盗梦空间"},
	{name: "傲慢与偏见2005", isDir: true, entries: one("Pride.and.Prejudice.2005.mkv"), kind: "movie", title: "傲慢与偏见", year: 2005},
	{name: "流浪地球2", isDir: true, entries: one("The.Wandering.Earth.II.2023.mkv"), kind: "movie", title: "流浪地球2"},
	{name: "1917", isDir: true, entries: one("1917.2019.2160p.mkv"), kind: "movie", title: "1917"},
	{name: "铁血战士：杀戮之地", isDir: true, entries: one("Predator.Badlands.2025.mkv"), kind: "movie", title: "铁血战士：杀戮之地"},
	{name: "冰河时代1", isDir: true, entries: one("Ice.Age.2002.mkv"), kind: "movie", title: "冰河时代1"},
	{name: "Spirited Away (2001)", isDir: true, entries: cat(one("Spirited.Away.2001.mkv"), dirs("Subs")), kind: "movie", title: "Spirited Away", year: 2001},

	// --- shows ---
	{name: "怪奇物语第五季", isDir: true, entries: eps("Stranger.Things.S05E%02d.mkv", 8), kind: "tv", title: "怪奇物语", season: 5},
	{name: "行尸走肉", isDir: true, entries: dirs("Season 1", "Season 2", "Season 3", "Season 4", "Season 5", "Season 6", "Season 7", "Season 8", "Season 9", "Season 10", "Season 11"), kind: "tv", title: "行尸走肉"},
	{name: "绝望主妇(2004)", isDir: true, entries: dirs("Season 1", "Season 2", "Season 3"), kind: "tv", title: "绝望主妇", year: 2004},
	{name: "三国演义94版", isDir: true, entries: dirs("第1部（群雄逐鹿）", "第2部（赤壁鏖战）", "第3部（三足鼎立）"), kind: "tv", title: "三国演义", year: 1994},
	{name: "繁花", isDir: true, entries: eps("繁花.Blossoms.Shanghai.E%02d.2160p.mkv", 30), kind: "tv", title: "繁花"},
	{name: "小丑回魂：欢迎来到德里镇", isDir: true, entries: dirs("Season 1"), kind: "tv", title: "小丑回魂：欢迎来到德里镇"},
	{name: "绝命毒师【S01-S05+电影】蓝光原盘REMUX 原盘中字", isDir: true,
		entries: dirs("Season 1", "Season 2", "Season 3", "Season 4", "Season 5", "El Camino"), kind: "tv", title: "绝命毒师", season: 1},
	{name: "凉宫春日的忧郁[nyaa][内嵌中日]", isDir: true, entries: eps("[Nekomoe] 凉宫春日的忧郁 - %02d [BDRip][1080p].mkv", 14), kind: "tv", title: "凉宫春日的忧郁"},
	{name: "Fate Zero", isDir: true, entries: eps("Fate Zero - %02d.mkv", 25), kind: "tv", title: "Fate Zero"},
	{name: "魔法少女小圆", isDir: true, entries: eps("魔法少女小圆 第%02d话.mkv", 12), kind: "tv", title: "魔法少女小圆"},
	{name: "[ANi] 葬送的芙莉莲 [01-28][1080P][Baha]", isDir: true, entries: eps("[ANi] 葬送的芙莉莲 - %02d [1080P][Baha].mkv", 28), kind: "anime", title: "葬送的芙莉莲"},
	{name: "[喵萌奶茶屋&LoliHouse] 间谍过家家 - 01-12 [WebRip 1080p HEVC-10bit AAC]", isDir: true,
		entries: eps("[喵萌奶茶屋&LoliHouse] 间谍过家家 - %02d [WebRip 1080p HEVC-10bit AAC].mkv", 12), kind: "anime", title: "间谍过家家"},

	// --- collections ---
	{name: "昆汀合集", isDir: true, entries: dirs("Pulp Fiction (1994)", "Reservoir Dogs (1992)", "Django Unchained (2012)", "Jackie Brown (1997)"), kind: "collection", title: "昆汀"},
	{name: "周星驰", isDir: true, entries: dirs("大话西游", "喜剧之王", "少林足球", "功夫"), kind: "collection", title: "周星驰"},
	{name: "大决战三部曲", isDir: true, entries: dirs("辽沈战役", "淮海战役", "平津战役"), kind: "collection", title: "大决战"},
	{name: "无间道三部曲", isDir: true, entries: dirs("无间道", "无间道II", "无间道III"), kind: "collection", title: "无间道"},
	{name: "教父合集", isDir: true, entries: dirs("The Godfather (1972)", "The Godfather Part II (1974)", "The Godfather Part III (1990)"), kind: "collection", title: "教父"},
	{name: "EVA合集", isDir: true, entries: dirs("新世纪福音战士 TV版", "新世纪福音战士 剧场版 序", "新世纪福音战士 剧场版 破"), kind: "collection", title: "EVA"},
	{name: "特别的她 FLCL 合集", isDir: true, entries: dirs("特别的她", "特别的她 进步", "特别的她 另类"), kind: "collection", title: "特别的她"},

	// --- not media ---
	{name: "黑神话.悟空.OST", isDir: true, entries: vids("01.mp3")[:0:0], kind: "ignore"},
	{name: "人人网字幕库", isDir: true, entries: []Entry{{Name: "a.srt"}, {Name: "b.ass"}, {Name: "c.zip"}}, kind: "ignore"},
}

func TestRealNameSamples(t *testing.T) {
	if len(samples) < 30 {
		t.Fatalf("only %d samples, want at least 30", len(samples))
	}
	ok := 0
	for _, s := range samples {
		t.Run(s.name, func(t *testing.T) {
			entries := s.entries
			if s.isDir && entries == nil {
				entries = []Entry{}
			}
			r := Classify(s.name, s.isDir, entries)
			var bad []string
			if r.Kind != s.kind {
				bad = append(bad, fmt.Sprintf("kind %q want %q (%s)", r.Kind, s.kind, r.Why))
			}
			if s.kind != "ignore" {
				if r.Title != s.title {
					bad = append(bad, fmt.Sprintf("title %q want %q", r.Title, s.title))
				}
				if r.Year != s.year {
					bad = append(bad, fmt.Sprintf("year %d want %d", r.Year, s.year))
				}
				if r.Season != s.season {
					bad = append(bad, fmt.Sprintf("season %d want %d", r.Season, s.season))
				}
				if r.Theatrical != s.theatrical {
					bad = append(bad, fmt.Sprintf("theatrical %v want %v", r.Theatrical, s.theatrical))
				}
			}
			if len(bad) > 0 {
				t.Errorf("%v", bad)
				return
			}
			ok++
		})
	}
	t.Logf("identification: %d of %d samples match the expected kind and title", ok, len(samples))
}

func TestOSTDirectoryWithAudio(t *testing.T) {
	r := Classify("黑神话.悟空.OST", true, []Entry{{Name: "01.flac"}, {Name: "02.flac"}})
	if r.Kind != "ignore" {
		t.Fatalf("kind = %q", r.Kind)
	}
}

func TestNumberedSeriesRule(t *testing.T) {
	cases := []struct {
		names []string
		want  bool
	}{
		{[]string{"Show - 01", "Show - 02", "Show - 03"}, true},
		{[]string{"Movie.1999", "Movie.2001"}, false}, // years are not numbering
		{[]string{"无间道", "无间道II", "无间道III"}, false},
		{[]string{"The Godfather (1972)", "The Godfather Part II (1974)"}, false},
		{[]string{"第1部（群雄逐鹿）", "第2部（赤壁鏖战）"}, true},
		{[]string{"A", "B"}, false},
	}
	for _, c := range cases {
		if got := numberedSeries(c.names); got != c.want {
			t.Errorf("numberedSeries(%v) = %v, want %v", c.names, got, c.want)
		}
	}
}

func TestEpisodePatternsTruncateBeforeEpisodeTitle(t *testing.T) {
	m, ok := episodeOf("Stranger.Things.S05E08.Chapter.Eight.The.Rightside.Up.mkv")
	if !ok || m.Season != 5 || m.Episode != 8 || m.Cut != len("Stranger.Things.") {
		t.Fatalf("got %+v ok=%v", m, ok)
	}
	if m, ok := episodeOf("三国演义【20.孙 策 之 死】The Romance of Three Kingdoms.1994.mkv"); !ok || m.Episode != 20 {
		t.Fatalf("bracket episode: %+v %v", m, ok)
	}
	if _, ok := episodeOf("Inception.2010.mkv"); ok {
		t.Fatal("a year was read as an episode")
	}
}

func TestUnlistedDirectoryIsLowConfidence(t *testing.T) {
	r := Classify("某种物质", true, nil)
	if r.Kind != "movie" || r.Confidence >= 0.6 {
		t.Fatalf("unlisted bare name = %+v; want a movie guess below the 0.6 line", r)
	}
	r = Classify("昆汀合集", true, nil)
	if r.Kind != "collection" {
		t.Fatalf("collection word ignored: %+v", r)
	}
}

func TestInlineIDTags(t *testing.T) {
	for _, c := range []struct {
		name       string
		isDir      bool
		title      string
		year       int
		tmdb, imdb string
	}{
		{"魔法少女小圆 (2011) {tmdb-39218}", true, "魔法少女小圆", 2011, "39218", ""},
		{"杀戮都市 (2004) {tmdb-11279}", true, "杀戮都市", 2004, "11279", ""},
		{"Fight Club (1999) {tmdbid-550}", true, "Fight Club", 1999, "550", ""},
		{"Fight Club (1999) [tmdbid=550]", true, "Fight Club", 1999, "550", ""},
		{"Se7en (1995) {imdb-tt0114369}", true, "Se7en", 1995, "", "tt0114369"},
		{"Se7en (1995) [imdbid-tt0114369]", true, "Se7en", 1995, "", "tt0114369"},
		{"盗梦空间 (2010) {tmdb-27205}.mkv", false, "盗梦空间", 2010, "27205", ""},
		{"Alien (1979) {tmdb-348} {imdb-tt0078748}", true, "Alien", 1979, "348", "tt0078748"},
		{"魔法少女小圆 (2011)", true, "魔法少女小圆", 2011, "", ""},
	} {
		r := Classify(c.name, c.isDir, []Entry{})
		if r.TMDBID != c.tmdb || r.IMDbID != c.imdb || r.Title != c.title || r.Year != c.year {
			t.Errorf("%s: tmdb=%q imdb=%q title=%q year=%d", c.name, r.TMDBID, r.IMDbID, r.Title, r.Year)
		}
	}
	if clean, tm, im := ExtractIDTags("X {tmdb-abc} {imdb-12}"); tm != "" || im != "" || clean != "X {tmdb-abc}" {
		t.Errorf("malformed tags: %q %q %q", clean, tm, im)
	}
}

func TestYearFirstNamesAndResolutionTokens(t *testing.T) {
	for _, c := range []struct {
		name  string
		title string
		year  int
	}{
		{"1995.Toy.Story.1920x1080.BDRip.x264.DTS-HD.MA.mkv", "Toy Story", 1995},
		{"2001.Monsters,.Inc..1920x1036.BDRip.x264.DTS-HD.MA.mkv", "Monsters, Inc", 2001},
		{"2008.WALL-E.1920x802.BDRip.x264.DTS-HD.MA.mkv", "WALL E", 2008},
		{"1995 Toy Story 1920x1080 BDRip.mkv", "Toy Story", 1995},
		{"2008.瓦力.WALL-E.1080p.mkv", "瓦力", 2008},
		// numbers that are part of the title stay in it
		{"1917.2019.1080p.BluRay.mkv", "1917", 2019},
		{"1917.mkv", "1917", 0},
		{"1917 (2019).mkv", "1917", 2019},
		{"2001.A.Space.Odyssey.1968.1080p.mkv", "2001 A Space Odyssey", 1968},
		{"300.斯巴达勇士.2006.mkv", "斯巴达勇士", 2006},
		{"The.Dark.Knight.2008.1080p.BluRay.mkv", "The Dark Knight", 2008},
	} {
		r := Classify(c.name, false, nil)
		if r.Kind != "movie" || r.Title != c.title || r.Year != c.year {
			t.Errorf("%s: kind=%s title=%q year=%d, want %q %d", c.name, r.Kind, r.Title, r.Year, c.title, c.year)
		}
		if len(r.Terms()) == 0 {
			t.Errorf("%s: no search terms", c.name)
		}
	}
}
