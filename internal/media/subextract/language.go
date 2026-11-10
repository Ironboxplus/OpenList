package subextract

import "strings"

// languageGroups lists the codes one language goes by: ISO 639-2/B, 639-2/T and
// 639-1, for the languages that matter here. The first is the one reported. A
// code in no group stands for itself.
var languageGroups = [][]string{
	{"chi", "zho", "zh", "cmn", "yue"}, {"eng", "en"}, {"jpn", "ja"}, {"kor", "ko"},
	{"spa", "es"}, {"fre", "fra", "fr"}, {"ger", "deu", "de"}, {"ita", "it"},
	{"por", "pt"}, {"rus", "ru"}, {"ara", "ar"}, {"tha", "th"}, {"vie", "vi"},
	{"may", "msa", "ms"}, {"ind", "id"}, {"hin", "hi"}, {"tur", "tr"}, {"pol", "pl"},
	{"dut", "nld", "nl"}, {"swe", "sv"}, {"dan", "da"}, {"nor", "no", "nob"}, {"fin", "fi"},
	{"cze", "ces", "cs"}, {"gre", "ell", "el"}, {"heb", "he"}, {"hun", "hu"}, {"ukr", "uk"},
}

// LanguageKey is the code a language is compared and reported by: the first of
// its group (ISO 639-2/B, which FFmpeg and mkvmerge list), or the primary subtag
// of a code that is in none ("ko-KR" is "ko").
func LanguageKey(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if i := strings.IndexAny(l, "-_"); i > 0 {
		l = l[:i]
	}
	for _, g := range languageGroups {
		for _, code := range g {
			if code == l {
				return g[0]
			}
		}
	}
	return l
}

// SameLanguage compares two language codes; one that is missing or "und" says
// nothing, so it does not disagree.
func SameLanguage(a, b string) bool {
	ka, kb := LanguageKey(a), LanguageKey(b)
	if ka == "" || kb == "" || ka == "und" || kb == "und" {
		return true
	}
	return ka == kb
}
