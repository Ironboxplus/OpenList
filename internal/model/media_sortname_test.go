package model

import (
	"slices"
	"strings"
	"testing"
)

func TestMediaSortName(t *testing.T) {
	for in, want := range map[string]string{
		"The Matrix":       "matrix",
		"A Beautiful Mind": "beautiful mind",
		"Movie 2":          "movie 0000000002",
		"Movie 10":         "movie 0000000010",
		"Amélie":           "amelie",
		"中文 电影":            "zhongwen dianying",
		"007：诺博士":          "0000000007 nuoboshi",
		"Back,to,School":   "back to school",
		"   ":              "",
	} {
		if got := MediaSortName(in); got != want {
			t.Errorf("MediaSortName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMediaSortNameOrdersChineseByPinyinAndNumbersByValue(t *testing.T) {
	titles := []string{"周星驰", "阿凡达", "Movie 10", "Movie 2", "宝贝计划", "The Abyss"}
	slices.SortFunc(titles, func(a, b string) int { return strings.Compare(MediaSortName(a), MediaSortName(b)) })
	want := []string{"The Abyss", "阿凡达", "宝贝计划", "Movie 2", "Movie 10", "周星驰"}
	if !slices.Equal(titles, want) {
		t.Fatalf("got %v, want %v", titles, want)
	}
}

func TestMediaSortNameFitsItsColumn(t *testing.T) {
	if n := len(MediaSortName(strings.Repeat("长篇电影", 100))); n > MediaSortNameMax {
		t.Fatalf("key is %d bytes, the cap is %d", n, MediaSortNameMax)
	}
}
