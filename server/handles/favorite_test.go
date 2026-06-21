package handles

import "testing"

func TestNormalizeFavoritePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "/"},
		{"whitespace only", "   ", "/"},
		{"already clean", "/a/b", "/a/b"},
		{"missing leading slash", "a/b", "/a/b"},
		{"backslashes", "\\a\\b", "/a/b"},
		{"trailing slash", "/a/b/", "/a/b"},
		{"dot segments", "/a/../b", "/b"},
		{"root", "/", "/"},
		{"trim spaces", "  /a/b  ", "/a/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeFavoritePath(tc.in); got != tc.want {
				t.Fatalf("normalizeFavoritePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeFavoritePathDedup verifies that two inputs that should refer to
// the same favorite normalize to the same key (so toggle/dedup matches them).
func TestNormalizeFavoritePathDedup(t *testing.T) {
	a := normalizeFavoritePath("a/b/")
	b := normalizeFavoritePath("\\a\\b")
	if a != b {
		t.Fatalf("expected equal normalized paths, got %q and %q", a, b)
	}
}
