package scraper

import (
	"strconv"
	"strings"
)

// ParsedName is a release name split into its Chinese and English titles.
type ParsedName struct {
	English string
	Chinese string
	Extra   []string // other Chinese fragments, tried after the main title
	Year    string
	// BracketChinese are the Chinese titles found inside [] / 【】.
	BracketChinese []string
}

// ParseName splits a file or directory name into titles and a year, using the
// ported dev-media rules (leading serial numbers, bracketed Chinese titles,
// noise tokens).
func ParseName(name string) ParsedName {
	p := parseVideoFileName(name)
	return ParsedName{
		English: p.EnglishTitle, Chinese: p.ChineseTitle, Extra: p.ExtraChineseTitles,
		Year: p.Year, BracketChinese: extractBracketChinese(stripLeadingSerial(name)),
	}
}

// ParsedForSearch rebuilds the internal form from an already cleaned name.
func (p ParsedName) internal() parsedVideoTitle {
	return parsedVideoTitle{EnglishTitle: p.English, ChineseTitle: p.Chinese, ExtraChineseTitles: p.Extra, Year: p.Year}
}

// SearchTerm is one query to try, with its language and how much to trust it.
type SearchTerm = TitleCandidate

// SearchTerms lists the queries to try for a parsed name, best first.
func SearchTerms(p ParsedName) []SearchTerm {
	return extractCandidates(p.internal(), p.BracketChinese)
}

// BuildTitleCandidates is dev-media's seven-step candidate generator.
func BuildTitleCandidates(title string) []string { return buildTitleCandidates(title) }

// PickEndpoint says "tv" when the raw name carries a season/episode marker.
func PickEndpoint(raw string) string { return pickEndpoint(raw) }

// TitleSimilarity is the 0-1 similarity used for ranking hits.
func TitleSimilarity(a, b string) float32 { return titleSimilarity(a, b) }

// NormalizeTitle drops brackets, noise words and separators.
func NormalizeTitle(s string) string { return normalizeTitle(s) }

// IsNoiseToken reports whether a whole fragment is release noise (x264, 1080p ...).
func IsNoiseToken(s string) bool { return isNoiseToken(s) }

// Match thresholds from dev-media.
const (
	StrongMatch   = matchConfidenceThreshold
	FallbackMatch = fallbackThreshold
	LastResort    = desperateThreshold
)

// scoreParts is dev-media's scoreMatch with the pieces handed in, so that
// providers with other field names (Bangumi) share the weighting:
// 0.5*title + 0.3*year + 0.2*vote.
func scoreParts(sim float32, queryYear, hitYear int, vote float32) float32 {
	var yearScore float32
	switch {
	case queryYear == 0:
		yearScore = 0.4
	case hitYear == 0:
		yearScore = 0.2
	default:
		switch absInt(hitYear - queryYear) {
		case 0:
			yearScore = 1
		case 1:
			yearScore = 0.5
		case 2:
			yearScore = 0.2
		}
	}
	v := vote / 10
	if v > 1 {
		v = 1
	}
	if v < 0 {
		v = 0
	}
	return 0.5*sim + 0.3*yearScore + 0.2*v
}

func yearOf(date string) int {
	if len(date) >= 4 {
		if n, err := strconv.Atoi(date[:4]); err == nil {
			return n
		}
	}
	return 0
}

func atoiSafe(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
