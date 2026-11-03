package model

import (
	"strings"
	"unicode"

	"github.com/mozillazg/go-pinyin"
	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"
)

// MediaSortName is the key a library is ordered by, built after Jellyfin's
// BaseItem.GetSortName (MediaBrowser.Controller/Entities/BaseItem.cs): lower
// case, the words the/a/an dropped, runs of digits zero-padded to 10 so that
// "2" sorts before "10", and non-ASCII text transliterated (Jellyfin's
// Transliterated; here Han characters become toneless pinyin through
// github.com/mozillazg/go-pinyin, other accents are stripped). Punctuation is
// a space, which is wider than Jellyfin's SortRemoveCharacters/
// SortReplaceCharacters lists: a title like "007：诺博士" must sort on its
// words, not on the colon.
func MediaSortName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if s == "" {
		return ""
	}
	var words strings.Builder
	var buf strings.Builder
	flushWord := func() {
		if buf.Len() > 0 {
			words.WriteString(buf.String())
			words.WriteByte(' ')
			buf.Reset()
		}
	}
	args := pinyin.NewArgs()
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) && unicode.Is(unicode.Han, r):
			if py := pinyin.SinglePinyin(r, args); len(py) > 0 {
				buf.WriteString(py[0]) // the syllables of a word run together: "zhongwen"
			} else {
				buf.WriteRune(r)
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r):
			buf.WriteRune(r)
		default:
			flushWord()
		}
	}
	flushWord()

	toks := strings.Fields(words.String())
	var out []string
	for i, t := range toks {
		// the leading article only (Jellyfin also drops them inside a title)
		if (t == "the" || t == "a" || t == "an") && len(toks) > 1 && i == 0 {
			continue
		}
		out = append(out, padDigits(stripMarks(t)))
	}
	key := strings.Join(out, " ")
	if len(key) > MediaSortNameMax {
		key = key[:MediaSortNameMax]
	}
	return key
}

// MediaSortNameMax keeps the key inside its column (size:191).
const MediaSortNameMax = 180

// padDigits left-pads every run of ASCII digits to 10 (Jellyfin ModifySortChunks).
func padDigits(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j > i {
			if j-i < 10 {
				b.WriteString(strings.Repeat("0", 10-(j-i)))
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// stripMarks removes combining accents ("é" -> "e").
func stripMarks(s string) string {
	d := norm.NFD.String(s)
	var b strings.Builder
	for _, r := range d {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SortTitle is what a work is ordered by: its display title, else its file name.
func (m *MediaItem) SortTitle() string {
	if t := strings.TrimSpace(m.ScrapedName); t != "" {
		return t
	}
	return m.FileName
}

// BeforeSave keeps SortName in step with the title whenever the row is written.
func (m *MediaItem) BeforeSave(_ *gorm.DB) error {
	m.SortName = MediaSortName(m.SortTitle())
	return nil
}
