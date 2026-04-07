package frontend

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
)

// createTestTarGz creates a tar.gz containing files with given names and contents
func createTestTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		gw := gzip.NewWriter(pw)
		defer gw.Close()
		tw := tar.NewWriter(gw)
		defer tw.Close()
		for name, content := range files {
			hdr := &tar.Header{
				Name: name,
				Mode: 0644,
				Size: int64(len(content)),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return
			}
			if _, err := tw.Write([]byte(content)); err != nil {
				return
			}
		}
	}()
	data, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("read tar.gz: %v", err)
	}
	return data
}

func TestExtractTarGz(t *testing.T) {
	tmpDir := t.TempDir()
	files := map[string]string{
		"dist/index.html":       "<html>hello</html>",
		"dist/assets/app.js":    "console.log('app')",
		"dist/assets/style.css": "body {}",
		"dist/images/logo.svg":  "<svg></svg>",
	}
	tarData := createTestTarGz(t, files)

	err := extractTarGz(strings.NewReader(string(tarData)), tmpDir)
	if err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}

	for name, expectedContent := range files {
		path := filepath.Join(tmpDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(data) != expectedContent {
			t.Errorf("content of %s: got %q, want %q", name, string(data), expectedContent)
		}
	}
}

func TestExtractTarGzDotSlash(t *testing.T) {
	tmpDir := t.TempDir()
	files := map[string]string{
		"./dist/index.html": "<html>dot-slash</html>",
		"./":                "",
	}
	tarData := createTestTarGz(t, files)

	err := extractTarGz(strings.NewReader(string(tarData)), tmpDir)
	if err != nil {
		t.Fatalf("extractTarGz with ./ prefix: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "dist", "index.html"))
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	if string(data) != "<html>dot-slash</html>" {
		t.Errorf("got %q, want dot-slash content", string(data))
	}
}

func TestExtractTarGzPathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	files := map[string]string{
		"../../../etc/passwd": "root:x:0:0",
	}
	tarData := createTestTarGz(t, files)

	err := extractTarGz(strings.NewReader(string(tarData)), tmpDir)
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
	if !strings.Contains(err.Error(), "path traversal") {
		t.Errorf("expected path traversal error, got: %v", err)
	}
}

func TestHasValidDist(t *testing.T) {
	if HasValidDist() {
		t.Log("HasValidDist returned true (may have existing dist from previous runs)")
	}
}

func TestWriteAndReadVersion(t *testing.T) {
	_ = os.MkdirAll(GetDistPath(), 0755)
	versionPath := GetVersionFilePath()

	origData, origErr := os.ReadFile(versionPath)
	defer func() {
		if origErr == nil {
			_ = os.WriteFile(versionPath, origData, 0644)
		} else {
			_ = os.Remove(versionPath)
		}
	}()

	testVersion := "v1.0.0-test"
	if err := writeVersion(testVersion); err != nil {
		t.Fatalf("writeVersion: %v", err)
	}

	got := ReadCurrentVersion()
	if got != testVersion {
		t.Errorf("ReadCurrentVersion: got %q, want %q", got, testVersion)
	}
}

func TestShouldAutoFetch(t *testing.T) {
	origVersion := conf.WebVersion
	defer func() { conf.WebVersion = origVersion }()

	tests := []struct {
		version string
		want    bool
	}{
		{"", true},
		{"rolling", true},
		{"beta", true},
		{"dev", true},
		{"v3.0.0", false},
		{"latest", false},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			conf.WebVersion = tt.version
			if got := shouldAutoFetch(); got != tt.want {
				t.Errorf("shouldAutoFetch(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestFetchFromRollingIntegration(t *testing.T) {
	files := map[string]string{
		"./dist/index.html":     "<html>integration</html>",
		"./dist/assets/test.js": "console.log('test')",
	}
	tarData := createTestTarGz(t, files)

	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/OpenListTeam/OpenList-Frontend/releases/tags/rolling":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(fmt.Sprintf(`{
				"tag_name": "rolling-test",
				"assets": [{
					"name": "openlist-frontend-dist.tar.gz",
					"browser_download_url": "%s/download/frontend.tar.gz"
				}]
			}`, ts.URL)))
		case "/repos/OpenListTeam/OpenList-Frontend/git/ref/tags/rolling-test":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(`{
				"object": {
					"type": "commit",
					"sha": "0123456789abcdef0123456789abcdef01234567"
				}
			}`))
		case "/download/frontend.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			w.WriteHeader(200)
			w.Write(tarData)
		default:
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()

	ResetFetchState()

	destDir := GetDistPath()
	os.RemoveAll(filepath.Join(destDir, distDirName))
	os.Remove(GetVersionFilePath())

	ctx := context.Background()
	result, err := fetchFromTag(ctx, "rolling", ts.URL)
	if err != nil {
		t.Fatalf("fetchFromTag: %v", err)
	}

	if result.Version != "rolling-test@0123456789ab" {
		t.Errorf("version: got %q, want %q", result.Version, "rolling-test@0123456789ab")
	}
	if !result.Downloaded {
		t.Error("expected Downloaded=true")
	}

	idx, err := os.ReadFile(filepath.Join(result.DistPath, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if string(idx) != "<html>integration</html>" {
		t.Errorf("index.html: got %q", string(idx))
	}

	ver := ReadCurrentVersion()
	expectedVer := "rolling-test@0123456789ab"
	if ver != expectedVer {
		t.Errorf("version file: got %q, want %q", ver, expectedVer)
	}
}

func TestResolveTagCommitSHA_AnnotatedTag(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/OpenListTeam/OpenList-Frontend/git/ref/tags/rolling":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{
				"object": {
					"type": "tag",
					"sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				}
			}`))
		case "/repos/OpenListTeam/OpenList-Frontend/git/tags/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{
				"object": {
					"type": "commit",
					"sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				}
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	sha, err := resolveTagCommitSHA(context.Background(), ts.Client(), ts.URL, "rolling")
	if err != nil {
		t.Fatalf("resolveTagCommitSHA: %v", err)
	}
	if sha != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("sha: got %q, want %q", sha, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	}
}

func TestVersionIdentifierFallback(t *testing.T) {
	tests := []struct {
		name     string
		tag      string
		sha      string
		fallback string
		want     string
	}{
		{name: "hash preferred", tag: "rolling", sha: "0123456789abcdef", fallback: "fallback", want: "rolling@0123456789ab"},
		{name: "fallback url", tag: "rolling", sha: "", fallback: "http://example.com/dist.tar.gz", want: "http://example.com/dist.tar.gz"},
		{name: "tag only", tag: "rolling", sha: "", fallback: "", want: "rolling"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := versionIdentifier(tt.tag, tt.sha, tt.fallback)
			if got != tt.want {
				t.Fatalf("versionIdentifier() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveTagCommitSHA_RealGitHubWithProxy10808(t *testing.T) {
	if os.Getenv("OPENLIST_REAL_GITHUB_TEST") != "1" {
		t.Skip("set OPENLIST_REAL_GITHUB_TEST=1 to run real GitHub integration test (proxy 127.0.0.1:10808 recommended)")
	}

	if os.Getenv("HTTP_PROXY") == "" && os.Getenv("http_proxy") == "" {
		_ = os.Setenv("HTTP_PROXY", "http://127.0.0.1:10808")
	}
	if os.Getenv("HTTPS_PROXY") == "" && os.Getenv("https_proxy") == "" {
		_ = os.Setenv("HTTPS_PROXY", "http://127.0.0.1:10808")
	}

	client := newHTTPClient()
	sha, err := resolveTagCommitSHA(context.Background(), client, "", "rolling")
	if err != nil {
		t.Fatalf("resolveTagCommitSHA(real): %v", err)
	}

	matched, _ := regexp.MatchString("^[0-9a-f]{40}$", sha)
	if !matched {
		t.Fatalf("sha format invalid: %q", sha)
	}

	// Optional sanity: ensure API can fetch release JSON in real scenario
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/repos/OpenListTeam/OpenList-Frontend/releases/tags/rolling", nil)
	if err != nil {
		t.Fatalf("create release request: %v", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "OpenList")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("fetch release: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("release API status=%d body=%s", resp.StatusCode, string(body))
	}

	var release map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		t.Fatalf("decode release: %v", err)
	}
	if release["tag_name"] == nil {
		t.Fatalf("release tag_name missing")
	}
}

func TestEnsureDistOnceFailureDoesNotLock(t *testing.T) {
	ResetFetchState()
	_ = os.MkdirAll(GetDistPath(), 0755)

	destDir := GetDistPath()
	os.RemoveAll(filepath.Join(destDir, distDirName))
	os.Remove(GetVersionFilePath())

	origVersion := conf.WebVersion
	conf.WebVersion = "v3.0.0" // shouldAutoFetch returns false
	defer func() { conf.WebVersion = origVersion }()

	ctx := context.Background()

	result := EnsureDistOnce(ctx)
	if result != "" {
		t.Errorf("expected empty result, got %q", result)
	}

	// Second call should also work (not locked by previous failure)
	result2 := EnsureDistOnce(ctx)
	if result2 != "" {
		t.Errorf("expected empty result on retry, got %q", result2)
	}
}
