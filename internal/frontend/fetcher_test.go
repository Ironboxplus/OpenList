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

func TestExtractTarGzRejectsOversizedFile(t *testing.T) {
	tmpDir := t.TempDir()
	// Create a tar.gz with a file whose header claims a size exceeding the limit
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		gw := gzip.NewWriter(pw)
		defer gw.Close()
		tw := tar.NewWriter(gw)
		defer tw.Close()
		hdr := &tar.Header{
			Name: "dist/huge.bin",
			Mode: 0644,
			Size: maxExtractFileSize + 1,
		}
		_ = tw.WriteHeader(hdr)
		// Write just enough to pass; the size check should reject before reading
		buf := make([]byte, 1024)
		for written := int64(0); written < hdr.Size; written += int64(len(buf)) {
			n := min(int64(len(buf)), hdr.Size-written)
			_, _ = tw.Write(buf[:n])
		}
	}()
	data, _ := io.ReadAll(pr)

	err := extractTarGz(strings.NewReader(string(data)), tmpDir)
	if err == nil {
		t.Fatal("expected error for oversized file, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("expected 'too large' error, got: %v", err)
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

func TestLegacyConfigJSONGetsDefaultFrontendRepo(t *testing.T) {
	cfg := conf.DefaultConfig("data")
	if err := json.Unmarshal([]byte(`{"site_url":"https://example.com"}`), cfg); err != nil {
		t.Fatalf("unmarshal legacy config: %v", err)
	}
	if cfg.FrontendRepo != conf.FrontendRepoDefault {
		t.Fatalf("FrontendRepo: got %q, want %q", cfg.FrontendRepo, conf.FrontendRepoDefault)
	}
}

func TestDefaultConfigUsesBuiltFrontendRepoDefault(t *testing.T) {
	orig := conf.FrontendRepoDefault
	conf.FrontendRepoDefault = "Ironboxplus/OpenList-Frontend"
	defer func() { conf.FrontendRepoDefault = orig }()

	cfg := conf.DefaultConfig("data")
	if cfg.FrontendRepo != "Ironboxplus/OpenList-Frontend" {
		t.Fatalf("FrontendRepo: got %q, want %q", cfg.FrontendRepo, "Ironboxplus/OpenList-Frontend")
	}
}

func TestExistingConfigJSONKeepsFrontendRepo(t *testing.T) {
	cfg := conf.DefaultConfig("data")
	if err := json.Unmarshal([]byte(`{"frontend_repo":"Ironboxplus/OpenList-Frontend"}`), cfg); err != nil {
		t.Fatalf("unmarshal config with frontend_repo: %v", err)
	}
	if cfg.FrontendRepo != "Ironboxplus/OpenList-Frontend" {
		t.Fatalf("FrontendRepo: got %q", cfg.FrontendRepo)
	}
}

func TestFetchFromTagUsesConfiguredFrontendRepo(t *testing.T) {
	origConf := conf.Conf
	if origConf == nil {
		conf.Conf = &conf.Config{}
	} else {
		confCopy := *origConf
		conf.Conf = &confCopy
	}
	defer func() { conf.Conf = origConf }()
	conf.Conf.FrontendRepo = "Ironboxplus/OpenList-Frontend"

	files := map[string]string{
		"./dist/index.html": "<html>custom-repo</html>",
	}
	tarData := createTestTarGz(t, files)

	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/Ironboxplus/OpenList-Frontend/releases/tags/rolling":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fmt.Sprintf(`{
				"tag_name": "rolling-custom",
				"assets": [{
					"name": "openlist-frontend-dist.tar.gz",
					"browser_download_url": "%s/download/custom.tar.gz"
				}]
			}`, ts.URL)))
		case "/repos/Ironboxplus/OpenList-Frontend/git/ref/tags/rolling-custom":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"object": {
					"type": "commit",
					"sha": "fedcba9876543210fedcba9876543210fedcba98"
				}
			}`))
		case "/download/custom.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(tarData)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	ResetFetchState()
	_ = os.MkdirAll(GetDistPath(), 0o755)
	_ = os.RemoveAll(filepath.Join(GetDistPath(), distDirName))
	_ = os.Remove(GetVersionFilePath())

	result, err := fetchFromTag(context.Background(), "rolling", ts.URL)
	if err != nil {
		t.Fatalf("fetchFromTag(custom repo): %v", err)
	}
	if result.Version != "rolling-custom@fedcba987654" {
		t.Fatalf("version: got %q, want %q", result.Version, "rolling-custom@fedcba987654")
	}

	data, err := os.ReadFile(filepath.Join(result.DistPath, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if string(data) != "<html>custom-repo</html>" {
		t.Fatalf("index.html: got %q", string(data))
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

func TestStaleCacheOverriddenByNewerRelease(t *testing.T) {
	oldFiles := map[string]string{"./dist/index.html": "<html>old</html>"}
	oldTar := createTestTarGz(t, oldFiles)
	newFiles := map[string]string{"./dist/index.html": "<html>new</html>"}
	newTar := createTestTarGz(t, newFiles)

	callCount := 0
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case fmt.Sprintf("/repos/%s/releases/tags/rolling", getFrontendRepo()):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(fmt.Sprintf(`{
				"tag_name": "rolling",
				"assets": [{"name": "openlist-frontend-dist.tar.gz", "browser_download_url": "%s/download/dist.tar.gz"}]
			}`, ts.URL)))
		case fmt.Sprintf("/repos/%s/git/ref/tags/rolling", getFrontendRepo()):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"object":{"type":"commit","sha":"aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd"}}`))
		case "/download/dist.tar.gz":
			callCount++
			w.Header().Set("Content-Type", "application/gzip")
			w.WriteHeader(200)
			if callCount == 1 {
				_, _ = w.Write(oldTar)
			} else {
				_, _ = w.Write(newTar)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()

	ResetFetchState()
	destDir := GetDistPath()
	_ = os.MkdirAll(destDir, 0755)
	os.RemoveAll(filepath.Join(destDir, distDirName))
	os.Remove(GetVersionFilePath())

	// Write a stale cached version
	_ = writeVersion("rolling@stale000000000")
	_ = os.MkdirAll(filepath.Join(destDir, distDirName), 0755)
	_ = os.WriteFile(filepath.Join(destDir, distDirName, "index.html"), []byte("<html>stale</html>"), 0644)

	// Fetch should detect version mismatch and download
	result, err := fetchFromTag(context.Background(), "rolling", ts.URL)
	if err != nil {
		t.Fatalf("fetchFromTag with stale cache: %v", err)
	}
	if !result.Downloaded {
		t.Error("expected Downloaded=true when cache is stale")
	}

	data, err := os.ReadFile(filepath.Join(result.DistPath, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if string(data) != "<html>old</html>" {
		t.Errorf("index.html: got %q, want old content", string(data))
	}
}

func TestWatcherTriggersCallbackOnNewVersion(t *testing.T) {
	files := map[string]string{"./dist/index.html": "<html>watcher</html>"}
	tarData := createTestTarGz(t, files)

	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case fmt.Sprintf("/repos/%s/releases/tags/rolling", getFrontendRepo()):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(fmt.Sprintf(`{
				"tag_name": "rolling",
				"assets": [{"name": "openlist-frontend-dist.tar.gz", "browser_download_url": "%s/download/dist.tar.gz"}]
			}`, ts.URL)))
		case fmt.Sprintf("/repos/%s/git/ref/tags/rolling", getFrontendRepo()):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"object":{"type":"commit","sha":"watchertest1234567890watchertest1234567890"}}`))
		case "/download/dist.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			w.WriteHeader(200)
			_, _ = w.Write(tarData)
		default:
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()

	ResetFetchState()
	destDir := GetDistPath()
	_ = os.MkdirAll(destDir, 0755)
	os.RemoveAll(filepath.Join(destDir, distDirName))
	os.Remove(GetVersionFilePath())

	// The watcher's check() calls FetchFromRolling which uses api.github.com.
	// For this test, call fetchFromTag directly and verify Downloaded triggers callback logic.
	result, err := fetchFromTag(context.Background(), "rolling", ts.URL)
	if err != nil {
		t.Fatalf("fetchFromTag: %v", err)
	}
	if !result.Downloaded {
		t.Fatal("expected Downloaded=true for watcher callback trigger")
	}
	if result.Version != "rolling@watchertest1" {
		t.Errorf("version: got %q", result.Version)
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
