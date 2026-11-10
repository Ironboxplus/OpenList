package subextract

import "testing"

func TestLanguageKeyReportsISO6392AndComparesAcrossSpellings(t *testing.T) {
	for in, want := range map[string]string{
		"en": "eng", "eng": "eng", "zh": "chi", "zho": "chi", "chi": "chi",
		"ko-KR": "kor", "fra": "fre", "xx-YY": "xx", " JA ": "jpn", "": "",
	} {
		if got := LanguageKey(in); got != want {
			t.Errorf("LanguageKey(%q) = %q, want %q", in, got, want)
		}
	}
	if !SameLanguage("zh", "chi") || !SameLanguage("und", "kor") || !SameLanguage("", "eng") {
		t.Error("codes of one language, or one that says nothing, must agree")
	}
	if SameLanguage("kor", "eng") {
		t.Error("kor and eng must not agree")
	}
}
