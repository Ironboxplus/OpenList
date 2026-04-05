package frontend

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	if result.Version != "rolling-test" {
		t.Errorf("version: got %q, want %q", result.Version, "rolling-test")
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
	expectedVer := ts.URL + "/download/frontend.tar.gz"
	if ver != expectedVer {
		t.Errorf("version file: got %q, want %q", ver, expectedVer)
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
