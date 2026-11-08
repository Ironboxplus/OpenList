package media

import (
	"context"
	"encoding/base64"
	"testing"
)

func TestLoadImageDataURIAndRejects(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}
	data, ct, err := LoadImage(context.Background(), "data:image/png;base64,"+base64.StdEncoding.EncodeToString(png))
	if err != nil || ct != "image/png" || string(data) != string(png) {
		t.Fatalf("data uri: %v %q", err, ct)
	}
	// the type is the bytes', not the declared one
	svg := base64.StdEncoding.EncodeToString([]byte("<svg onload=alert(1)/>"))
	for _, ref := range []string{
		"", "data:image/svg+xml;base64," + svg, "data:image/png,rawbytes", "http://127.0.0.1/a.jpg",
		"https://evil.example.com/a.jpg", "ftp://image.tmdb.org/a.jpg", "relative/path.jpg",
	} {
		if _, _, err := LoadImage(context.Background(), ref); err == nil {
			t.Errorf("%q should be refused", ref)
		}
	}
}

func TestBangumiImageHostIsAllowedButNotLookalikes(t *testing.T) {
	for host, want := range map[string]bool{
		"lain.bgm.tv": true, "bgm.tv": true, "image.tmdb.org": true,
		"evilbgm.tv": false, "bgm.tv.evil.com": false, "127.0.0.1": false,
	} {
		if got := hostAllowed(host); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}
