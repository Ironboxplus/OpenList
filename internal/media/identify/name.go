package identify

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/OpenListTeam/OpenList/v4/internal/media/scraper"
)

// NameInfo is what a release name says about itself, before any content is
// looked at.
type NameInfo struct {
	Title      string // preferred display/search title (Chinese if there is one)
	English    string
	Chinese    string
	Year       int
	Season     int  // first season named, 0 = none
	MultiSeas  bool // "S01-S05" style range
	Episodes   int  // announced episode count ("全8集", "[01-12]"), 0 = unknown
	Group      string
	Site       string // the publishing site tag that was stripped
	Anime      bool   // fansub-style naming
	TVHint     bool   // season / episode markers in the name
	Collection bool   // 合集 / 系列 / 三部曲 ...
	Theatrical bool   // 剧场版 / 映画 / Movie in the name
	TMDBID     string // inline {tmdb-123} tag: the number
	IMDbID     string // inline {imdb-tt0112159} tag
	Parsed     scraper.ParsedName
}

type segment struct {
	text    string
	bracket bool
}

var closers = map[rune]rune{'[': ']', '【': '】', '(': ')', '（': '）', '「': '」', '『': '』', '《': '》', '<': '>'}

func splitSegments(s string) []segment {
	var segs []segment
	var cur strings.Builder
	rs := []rune(s)
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			segs = append(segs, segment{text: t})
		}
		cur.Reset()
	}
	for i := 0; i < len(rs); i++ {
		if cl, ok := closers[rs[i]]; ok {
			j := i + 1
			for j < len(rs) && rs[j] != cl {
				j++
			}
			if j < len(rs) {
				flush()
				if t := strings.TrimSpace(string(rs[i+1 : j])); t != "" {
					segs = append(segs, segment{text: t, bracket: true})
				}
				i = j
				continue
			}
		}
		cur.WriteRune(rs[i])
	}
	flush()
	return segs
}

var (
	siteRe = regexp.MustCompile(`(?i)发布|www\.|\.com\b|\.net\b|\.cn\b|\.org\b|\.cc\b|\.top\b|\.me\b|https?:|论坛|字幕库|影视网|剧集网`)
	yearRe = regexp.MustCompile(`^(?:19|20)\d{2}$`)
	// techWordRe matches one separator-free word of release noise.
	techWordRe = regexp.MustCompile(`(?i)^(?:\d{3,4}[pi]|\d{3,4}[x×]\d{3,4}|\d+bit|ma\d+p|hi10p|hi422p|hevc|avc|flac|aac|ac3|eac3|dts|dd[p+]?\d*|truehd|atmos|bd|bdrip|bdmv|bluray|blu-ray|web|webrip|web-dl|dl|webdl|hdr\d*|dv|sdr|mkv|mp4|gb|big5|chs|cht|jpn|eng|jp|sc|tc|fin|end|v\d|ova|oad|sp|tv|tvrip|dvd|dvdrip|hdtv|remux|raw|raws|x26[45]|h26[45]|4k|2k|8k|uhd|nyaa|2160p|1080p|720p|480p|hd|fhd|av1|vp9|10bits?|8bits?|\d{1,3}(?:-\d{1,3})?(?:v\d)?|s\d{1,2}(?:-?s?\d{1,2})?(?:\+.*)?|e\d{1,3}|ep\d{1,3}|season\d*|complete|repack|proper|extended|uncut|imax|hdr10\+?|dolby|vision|atmos|hq|mp3|ass|srt|sub|subs|multi|dual|audio)$`)
	techHanRe  = regexp.MustCompile(`国语|粤语|国粤|国英|中字|简繁|简体|繁体|简日|繁日|简中|繁中|内嵌|内封|外挂|中日|中英|双语|双字|字幕|无字幕|全\s*\d+\s*[集话話]|\d+\s*[集话話]|完结|蓝光|原盘|高清|超清|修复|未删减|导演剪辑|剧场版|特别篇|合集|无修|特效|熟肉|生肉|日语|英语|韩语|国配|台配|港配|配音|音轨|粤配|多音轨|多字幕`)
	seasonRe   = regexp.MustCompile(`(?i)(?:\bS(\d{1,2})(?:\s*[-~]\s*S?(\d{1,2}))?(?:E\d{1,3})?\b)|(?:\bSeason[\s._-]*(\d{1,2})\b)|(?:第\s*([\d一二三四五六七八九十]+)\s*[季部期])`)
	epCountRe  = regexp.MustCompile(`全\s*(\d{1,3})\s*[集话話]|\b(\d{1,3})\s*[集话話]全|\[(?:\d{1,3})\s*[-~]\s*(\d{1,3})\]|\b\d{1,3}\s*-\s*(\d{1,3})\s*(?:fin|end|合集)?`)
	collRe     = regexp.MustCompile(`(?i)合集|系列|三部曲|四部曲|五部曲|全集|collection|trilogy|saga|anthology`)
	theatreRe  = regexp.MustCompile(`(?i)剧场版|劇場版|映画|\bmovie\b|the movie|电影版`)
	yearAttach = regexp.MustCompile(`^(.*\p{Han})((?:19|20)\d{2})$`)
	yy94Re     = regexp.MustCompile(`^(.*\p{Han})(\d{2})版?$`)
	// Western release groups, which share the [Group] shape with fansubs.
	westernGroup = regexp.MustCompile(`(?i)^(yts|yify|rarbg|evo|sparks|fgt|ettv|eztv|cmct|hds|mnhd|wiki|tlf|framestor|amiable|ntb|ntg|tigole|qxr|psa)(\.\w+)?$`)
	// A Chinese fansub group ends in 组/屋/社 or joins two groups with &.
	hanGroupRe = regexp.MustCompile(`字幕|汉化|工作室|制作组|压制组|发布组|组$|屋$|社$|&|\+`)
	ostRe      = regexp.MustCompile(`(?i)\bOST\b|原声带|原声大碟|配乐|soundtrack`)
	cnNumber   = map[rune]int{'一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9, '十': 10}
)

// idTagRe matches Jellyfin/Plex style tags: {tmdb-123}, {tmdbid-123},
// [tmdbid=123], {imdb-tt0112159}, [imdbid-tt0112159].
var idTagRe = regexp.MustCompile(`(?i)[\{\[\(]\s*(tmdb|imdb)(?:id)?\s*[-=:_ ]\s*(tt\d{5,}|\d+)\s*[\}\]\)]`)

// ExtractIDTags removes inline provider id tags from a name and returns them.
// A tmdb tag holds digits, an imdb tag "tt" and digits; a mismatched tag is
// removed but ignored.
func ExtractIDTags(name string) (clean, tmdb, imdb string) {
	clean = idTagRe.ReplaceAllStringFunc(name, func(m string) string {
		sm := idTagRe.FindStringSubmatch(m)
		id := strings.ToLower(sm[2])
		switch {
		case strings.EqualFold(sm[1], "tmdb") && isAllDigits(id):
			tmdb = id
		case strings.EqualFold(sm[1], "imdb") && strings.HasPrefix(id, "tt"):
			imdb = id
		}
		return " "
	})
	if clean != name {
		clean = strings.Join(strings.Fields(clean), " ")
	}
	return clean, tmdb, imdb
}

func cnToInt(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	rs := []rune(s)
	switch len(rs) {
	case 1:
		return cnNumber[rs[0]]
	case 2:
		if rs[0] == '十' {
			return 10 + cnNumber[rs[1]]
		}
	case 3:
		if rs[1] == '十' {
			return cnNumber[rs[0]]*10 + cnNumber[rs[2]]
		}
	}
	return 0
}

// isTechBracket reports whether a bracket holds only release noise.
func isTechBracket(s string) bool {
	if yearRe.MatchString(s) {
		return false
	}
	rest := techHanRe.ReplaceAllString(s, " ")
	toks := strings.FieldsFunc(rest, func(r rune) bool {
		return strings.ContainsRune(" _.,+/&-—~", r)
	})
	if len(toks) == 0 {
		return strings.TrimSpace(s) != "" && techHanRe.MatchString(s)
	}
	for _, t := range toks {
		if !techWordRe.MatchString(t) && !scraper.IsNoiseToken(t) {
			return false
		}
	}
	return true
}

func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func hasASCIILetter(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// ParseName reads a file or directory name. isFile strips the extension first.
func ParseName(name string, isFile bool) NameInfo {
	var ni NameInfo
	if isFile && IsVideo(name) {
		name = stem(name)
	}
	name, ni.TMDBID, ni.IMDbID = ExtractIDTags(name)
	ni.Collection = collRe.MatchString(name)
	ni.Theatrical = theatreRe.MatchString(name)

	segs := splitSegments(name)

	// 1. publishing-site tags anywhere
	kept := segs[:0:0]
	for _, s := range segs {
		if s.bracket && siteRe.MatchString(s.text) {
			ni.Site = s.text
			continue
		}
		kept = append(kept, s)
	}
	segs = kept
	// a bare "www.xxx.com" prefix in free text
	if len(segs) > 0 && !segs[0].bracket {
		if loc := regexp.MustCompile(`(?i)^\s*(?:www\.)?[a-z0-9-]+\.(?:com|net|cn|org|cc|top|me)\s*[-@_ ]*`).FindStringIndex(segs[0].text); loc != nil && loc[1] < len(segs[0].text) {
			ni.Site = strings.TrimSpace(segs[0].text[:loc[1]])
			segs[0].text = strings.TrimSpace(segs[0].text[loc[1]:])
		}
	}

	// 2. leading release group, e.g. [VCB-Studio]
	if len(segs) > 1 && segs[0].bracket && (!hasHan(segs[0].text) || hanGroupRe.MatchString(segs[0].text)) && len([]rune(segs[0].text)) <= 32 &&
		!yearRe.MatchString(segs[0].text) && !westernGroup.MatchString(segs[0].text) && !isTechBracket(segs[0].text) {
		ni.Group = segs[0].text
		ni.Anime = true
		segs = segs[1:]
	}

	// 3. hints from the rest of the brackets; remember the first title-like one
	titleBracket := ""
	var yearBracket int
	for _, s := range segs {
		if !s.bracket {
			continue
		}
		if yearRe.MatchString(s.text) {
			yearBracket, _ = strconv.Atoi(s.text)
			continue
		}
		if m := epCountRe.FindStringSubmatch(s.text); m != nil {
			for _, g := range m[1:] {
				if n, _ := strconv.Atoi(g); n > 0 {
					ni.Episodes = n
				}
			}
			if ni.Group != "" && regexp.MustCompile(`^\d{1,3}\s*[-~]\s*\d{1,3}$`).MatchString(s.text) {
				ni.Anime = true
			}
		}
		if isTechBracket(s.text) {
			if regexp.MustCompile(`(?i)bdrip|bd-?box|web-?rip|\bova\b|\boad\b|raws?`).MatchString(s.text) && ni.Group != "" {
				ni.Anime = true
			}
			continue
		}
		if titleBracket == "" && (hasHan(s.text) || hasLetter(s.text)) {
			titleBracket = s.text
		}
	}

	// 4. season / episode markers over the whole name
	if m := seasonRe.FindStringSubmatch(name); m != nil {
		ni.TVHint = true
		switch {
		case m[1] != "":
			ni.Season, _ = strconv.Atoi(m[1])
			ni.MultiSeas = m[2] != ""
		case m[3] != "":
			ni.Season, _ = strconv.Atoi(m[3])
		case m[4] != "":
			ni.Season = cnToInt(m[4])
		}
	}
	if ni.Episodes > 0 {
		ni.TVHint = true
	}
	if _, ok := episodeOf(name); ok {
		ni.TVHint = true
	}

	// 5. the title text: first free-text segment with real content
	core := ""
	for _, s := range segs {
		if s.bracket {
			continue
		}
		if t := cutNoise(s.text); t != "" && (hasHan(t) || hasLetter(t)) {
			core = t
			break
		}
	}
	if core == "" {
		core = titleBracket
	}
	core = strings.TrimSpace(core)

	// 6. years glued to Chinese titles: 傲慢与偏见2005, 三国演义94版
	year := yearBracket
	if m := yearAttach.FindStringSubmatch(core); m != nil {
		core = m[1]
		year, _ = strconv.Atoi(m[2])
	} else if m := yy94Re.FindStringSubmatch(core); m != nil {
		if n, _ := strconv.Atoi(m[2]); n >= 80 && n <= 99 {
			core = m[1]
			year = 1900 + n
		}
	}
	// season suffix glued to the title: 怪奇物语第五季
	if loc := regexp.MustCompile(`第\s*[\d一二三四五六七八九十]+\s*[季部期]`).FindStringIndex(core); loc != nil && loc[0] > 0 {
		core = strings.TrimSpace(core[:loc[0]])
	}
	// collection words are not part of the title
	if ni.Collection {
		core = strings.TrimSpace(collRe.ReplaceAllString(core, " "))
	}

	// 7. split Chinese / English, find a year in dotted release names
	if core != "" {
		p := scraper.ParseName(core + ".mkv")
		ni.Parsed = p
		ni.Chinese, ni.English = p.Chinese, p.English
		if year == 0 {
			year, _ = strconv.Atoi(p.Year)
		}
		// A purely Chinese title with spaces ("超时空要塞 可曾记得爱") is one
		// title; the ported parser would keep only its first word.
		if hasHan(core) && !hasASCIILetter(core) && !strings.Contains(core, ".") {
			ni.Chinese = core
			ni.Parsed.Chinese = core
		}
		// A Chinese title in brackets next to an English release name is the
		// better title: "19 [复仇者联盟3：无限战争]Avengers.Infinity.War.2018".
		if ni.Chinese == "" && titleBracket != "" && hasHan(titleBracket) && titleBracket != core && !isTechBracket(titleBracket) {
			ni.Chinese = titleBracket
			ni.Parsed.Chinese = titleBracket
		}
		switch {
		case ni.Chinese != "":
			ni.Title = ni.Chinese
		case ni.English != "":
			ni.Title = ni.English
		default:
			ni.Title = core
		}
	}
	ni.Year = year
	// a bare number is a title, not a year ("1917")
	if ni.Title == "" && core != "" {
		ni.Title = core
		if yearRe.MatchString(core) {
			ni.Year = 0
		}
	}
	if ni.Year > 0 {
		ni.Parsed.Year = strconv.Itoa(ni.Year)
	}
	return ni
}

// cutNoise trims a free-text fragment at its first release-noise word, so
// "Fight Club 1080p BluRay x264" becomes "Fight Club". Dotted release names
// ("Inception.2010.盗梦空间.双语字幕") are left whole for the ported parser.
func cutNoise(s string) string {
	s = strings.TrimSpace(s)
	if strings.Count(s, ".") >= 2 && !strings.Contains(s, " ") {
		return s
	}
	toks := strings.Fields(s)
	var out []string
	for i, t := range toks {
		if i > 0 && (techWordRe.MatchString(t) || scraper.IsNoiseToken(t) || techHanRe.MatchString(t)) {
			if !(len(t) <= 2 && isAllDigits(t)) { // a short number can be part of a title: "Steins;Gate 0"
				break
			}
		}
		out = append(out, t)
	}
	for len(out) > 0 && strings.Trim(out[len(out)-1], "-–—_~") == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, " ")
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
