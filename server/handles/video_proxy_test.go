package handles

import (
	"net/url"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
)

const testAPIURL = "http://openlist.test"

// seedSign installs a deterministic signing token in the setting cache so
// sign.Sign / sign.Verify round-trip without touching the database.
func seedSign(t *testing.T) {
	t.Helper()
	op.Cache.SetSetting(conf.LinkExpiration, &model.SettingItem{Key: conf.LinkExpiration, Value: "0"})
	op.Cache.SetSetting(conf.Token, &model.SettingItem{Key: conf.Token, Value: "test-token"})
	t.Cleanup(func() { op.Cache.ClearAll() })
}

// parseProxied pulls the upstream url + sign back out of a proxy link and
// asserts the signature is valid. Returns the decoded upstream URL.
func parseProxied(t *testing.T, proxied string) string {
	t.Helper()
	if !strings.HasPrefix(proxied, testAPIURL+videoProxyPath+"?") {
		t.Fatalf("proxied url has unexpected prefix: %s", proxied)
	}
	u, err := url.Parse(proxied)
	if err != nil {
		t.Fatalf("parse proxied url: %v", err)
	}
	raw := u.Query().Get("url")
	s := u.Query().Get("sign")
	if raw == "" || s == "" {
		t.Fatalf("proxied url missing url/sign params: %s", proxied)
	}
	if err := sign.Verify(raw, s); err != nil {
		t.Fatalf("sign verify failed for %q: %v", raw, err)
	}
	return raw
}

func TestBuildVideoProxyURL(t *testing.T) {
	seedSign(t)
	raw := "https://cdn.115.com/v/movie.m3u8?t=123&k=abc/def"
	got := BuildVideoProxyURL(testAPIURL, raw)

	// The upstream URL must be query-escaped (no bare '?'/'&' leaking past the
	// first '?'), and must round-trip + verify.
	if decoded := parseProxied(t, got); decoded != raw {
		t.Fatalf("round-trip mismatch:\n have %q\n want %q", decoded, raw)
	}
	if strings.Count(got, "?") != 1 {
		t.Fatalf("upstream query not escaped, multiple '?': %s", got)
	}
}

func TestIsM3U8(t *testing.T) {
	cases := []struct {
		ct, rawURL string
		want       bool
	}{
		{"application/vnd.apple.mpegurl", "https://x/y", true},
		{"application/x-mpegURL", "https://x/y", true},
		{"", "https://cdn.115.com/v/movie.m3u8", true},
		{"", "https://cdn.115.com/v/movie.m3u8?token=1", true},
		{"video/mp4", "https://cdn.115.com/v/movie.mp4", false},
		{"binary/octet-stream", "https://cdn.115.com/v/seg001.ts", false},
	}
	for _, c := range cases {
		if got := isM3U8(c.ct, c.rawURL); got != c.want {
			t.Errorf("isM3U8(%q,%q) = %v, want %v", c.ct, c.rawURL, got, c.want)
		}
	}
}

func TestRewriteM3U8(t *testing.T) {
	seedSign(t)
	base := "https://cdn.115.com/hls/720/index.m3u8?auth=xyz"
	manifest := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-KEY:METHOD=AES-128,URI=\"https://key.115.com/k?id=1\",IV=0x00",
		"#EXT-X-TARGETDURATION:10",
		"#EXTINF:9.009,",
		"seg001.ts",
		"#EXTINF:9.009,",
		"https://cdn.115.com/hls/720/seg002.ts?auth=xyz",
		"",
		"#EXT-X-ENDLIST",
	}, "\n")

	out := rewriteM3U8(manifest, base, testAPIURL)
	lines := strings.Split(out, "\n")

	// Structural tags preserved verbatim.
	for _, want := range []string{"#EXTM3U", "#EXT-X-VERSION:3", "#EXT-X-TARGETDURATION:10", "#EXTINF:9.009,", "#EXT-X-ENDLIST"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing preserved tag %q in output", want)
		}
	}
	// Blank line preserved (same line count as input).
	if len(lines) != len(strings.Split(manifest, "\n")) {
		t.Errorf("line count changed: got %d want %d", len(lines), len(strings.Split(manifest, "\n")))
	}

	// Relative segment resolves against the manifest base, then proxied.
	relLine := lines[5]
	if got := parseProxied(t, relLine); got != "https://cdn.115.com/hls/720/seg001.ts" {
		t.Errorf("relative segment resolved to %q", got)
	}
	// Absolute segment kept as-is, proxied.
	absLine := lines[7]
	if got := parseProxied(t, absLine); got != "https://cdn.115.com/hls/720/seg002.ts?auth=xyz" {
		t.Errorf("absolute segment resolved to %q", got)
	}

	// EXT-X-KEY URI rewritten in place (tag prefix kept, URI proxied).
	keyLine := lines[2]
	if !strings.HasPrefix(keyLine, "#EXT-X-KEY:METHOD=AES-128,URI=\"") || !strings.HasSuffix(keyLine, ",IV=0x00") {
		t.Fatalf("key line structure broken: %s", keyLine)
	}
	inner := keyLine[strings.Index(keyLine, "URI=\"")+len("URI=\"") : strings.LastIndex(keyLine, "\"")]
	if got := parseProxied(t, inner); got != "https://key.115.com/k?id=1" {
		t.Errorf("key URI proxied to %q", got)
	}

	// No raw provider CDN URL should survive outside an escaped 'url=' param.
	for i, l := range lines {
		if strings.HasPrefix(l, "#") {
			continue
		}
		if strings.Contains(l, "cdn.115.com") && !strings.Contains(l, "url=") {
			t.Errorf("line %d leaks raw CDN url: %s", i, l)
		}
	}
}
