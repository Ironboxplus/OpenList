package frontend

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

const (
	frontendRepo = "OpenListTeam/OpenList-Frontend"
	versionFile  = ".frontend_version"
	distDirName  = "dist"
)

// FetchResult contains the result of a fetch operation
type FetchResult struct {
	Version    string
	Downloaded bool
	DistPath   string
}

// GetDistPath returns the path where dynamically fetched frontend dist is stored
func GetDistPath() string {
	return filepath.Join(flags.DataDir, "frontend_dist")
}

// GetVersionFilePath returns the path to the version tracking file
func GetVersionFilePath() string {
	return filepath.Join(GetDistPath(), versionFile)
}

// HasValidDist checks if the dynamic dist directory exists and has an index.html
func HasValidDist() bool {
	distPath := GetDistPath()
	_, err := os.Stat(filepath.Join(distPath, distDirName, "index.html"))
	return err == nil
}

// ReadCurrentVersion reads the currently cached version from disk
func ReadCurrentVersion() string {
	data, err := os.ReadFile(GetVersionFilePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeVersion writes the version string to the version tracking file
func writeVersion(version string) error {
	return os.WriteFile(GetVersionFilePath(), []byte(version), 0644)
}

// FetchFromRolling downloads the frontend dist from the GitHub rolling release
func FetchFromRolling(ctx context.Context) (*FetchResult, error) {
	return fetchFromTag(ctx, "rolling", "")
}

// FetchFromLatest downloads the frontend dist from the GitHub latest release
func FetchFromLatest(ctx context.Context) (*FetchResult, error) {
	return fetchFromTag(ctx, "", "")
}

// githubRelease represents a GitHub release for JSON parsing
type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		BrowserDownloadURL string `json:"browser_download_url"`
		Name               string `json:"name"`
	} `json:"assets"`
	PublishedAt string `json:"published_at"`
}

type githubRef struct {
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

type githubAnnotatedTag struct {
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

func shortHash(sha string) string {
	const shortLen = 12
	if len(sha) > shortLen {
		return sha[:shortLen]
	}
	return sha
}

func versionIdentifier(tag, commitSHA, fallback string) string {
	if strings.TrimSpace(commitSHA) != "" {
		return fmt.Sprintf("%s@%s", tag, shortHash(commitSHA))
	}
	if strings.TrimSpace(fallback) != "" {
		return fallback
	}
	return tag
}

func resolveTagCommitSHA(ctx context.Context, client *http.Client, baseURL, tag string) (string, error) {
	if strings.TrimSpace(tag) == "" {
		return "", fmt.Errorf("empty tag")
	}

	apiBase := "https://api.github.com"
	if baseURL != "" {
		apiBase = strings.TrimRight(baseURL, "/")
	}

	refURL := fmt.Sprintf("%s/repos/%s/git/ref/tags/%s", apiBase, frontendRepo, url.PathEscape(tag))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, refURL, nil)
	if err != nil {
		return "", fmt.Errorf("create ref request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "OpenList")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch tag ref: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("tag ref API returned %d: %s", resp.StatusCode, string(body))
	}

	var ref githubRef
	if err := json.NewDecoder(resp.Body).Decode(&ref); err != nil {
		return "", fmt.Errorf("decode ref JSON: %w", err)
	}

	switch ref.Object.Type {
	case "commit":
		if ref.Object.SHA == "" {
			return "", fmt.Errorf("empty commit sha in ref response")
		}
		return ref.Object.SHA, nil
	case "tag":
		if ref.Object.SHA == "" {
			return "", fmt.Errorf("empty tag sha in ref response")
		}
		tagObjURL := fmt.Sprintf("%s/repos/%s/git/tags/%s", apiBase, frontendRepo, ref.Object.SHA)
		tagReq, err := http.NewRequestWithContext(ctx, http.MethodGet, tagObjURL, nil)
		if err != nil {
			return "", fmt.Errorf("create tag object request: %w", err)
		}
		tagReq.Header.Set("Accept", "application/vnd.github.v3+json")
		tagReq.Header.Set("User-Agent", "OpenList")

		tagResp, err := client.Do(tagReq)
		if err != nil {
			return "", fmt.Errorf("fetch tag object: %w", err)
		}
		defer tagResp.Body.Close()

		if tagResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(tagResp.Body)
			return "", fmt.Errorf("tag object API returned %d: %s", tagResp.StatusCode, string(body))
		}

		var tagObj githubAnnotatedTag
		if err := json.NewDecoder(tagResp.Body).Decode(&tagObj); err != nil {
			return "", fmt.Errorf("decode tag object JSON: %w", err)
		}
		if tagObj.Object.SHA == "" {
			return "", fmt.Errorf("empty object sha in tag object response")
		}
		return tagObj.Object.SHA, nil
	default:
		if ref.Object.SHA == "" {
			return "", fmt.Errorf("unsupported ref object type %q with empty sha", ref.Object.Type)
		}
		return ref.Object.SHA, nil
	}
}

// fetchFromTag downloads frontend dist from a GitHub release tag.
// If baseURL is non-empty, it replaces api.github.com (used for testing).
func fetchFromTag(ctx context.Context, tag string, baseURL string) (*FetchResult, error) {
	var apiURL string
	if baseURL != "" {
		if tag == "" {
			apiURL = fmt.Sprintf("%s/repos/%s/releases/latest", baseURL, frontendRepo)
		} else {
			apiURL = fmt.Sprintf("%s/repos/%s/releases/tags/%s", baseURL, frontendRepo, tag)
		}
	} else {
		if tag == "" {
			apiURL = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", frontendRepo)
		} else {
			apiURL = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", frontendRepo, tag)
		}
	}

	client := newHTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "OpenList")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github API returned %d: %s", resp.StatusCode, string(body))
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode release JSON: %w", err)
	}

	// Find the dist tarball URL (non-lite)
	var tarURL string
	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, "openlist-frontend-dist") &&
			!strings.Contains(asset.Name, "lite") &&
			strings.HasSuffix(asset.Name, ".tar.gz") {
			tarURL = asset.BrowserDownloadURL
			break
		}
	}
	if tarURL == "" {
		return nil, fmt.Errorf("no frontend dist tarball found in release %s", release.TagName)
	}

	commitSHA, err := resolveTagCommitSHA(ctx, client, baseURL, release.TagName)
	if err != nil {
		utils.Log.Warnf("[frontend] failed to resolve tag %s hash: %v", release.TagName, err)
	}

	resolvedVersion := versionIdentifier(release.TagName, commitSHA, tarURL)

	// Use tag+commit-hash as the primary version identifier.
	// For rolling releases the tag itself is static, but its target commit moves.
	// If hash resolve fails, fallback to tarball URL so updates can still be detected.
	currentVersion := ReadCurrentVersion()
	if currentVersion == resolvedVersion && HasValidDist() {
		utils.Log.Infof("[frontend] version %s already cached, skipping download", resolvedVersion)
		return &FetchResult{
			Version:    resolvedVersion,
			Downloaded: false,
			DistPath:   filepath.Join(GetDistPath(), distDirName),
		}, nil
	}

	utils.Log.Infof("[frontend] downloading version %s from %s", resolvedVersion, tarURL)
	if err := downloadAndExtract(ctx, client, tarURL); err != nil {
		return nil, fmt.Errorf("download and extract: %w", err)
	}

	if err := writeVersion(resolvedVersion); err != nil {
		utils.Log.Warnf("[frontend] failed to write version file: %v", err)
	}

	utils.Log.Infof("[frontend] successfully fetched version %s", resolvedVersion)
	return &FetchResult{
		Version:    resolvedVersion,
		Downloaded: true,
		DistPath:   filepath.Join(GetDistPath(), distDirName),
	}, nil
}

func downloadAndExtract(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("User-Agent", "OpenList")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download tarball: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	destDir := GetDistPath()
	tmpDir := filepath.Join(destDir, ".tmp-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := extractTarGz(resp.Body, tmpDir); err != nil {
		return fmt.Errorf("extract tar.gz: %w", err)
	}

	// Determine the source directory:
	// If the tarball contains a "dist" subdirectory, use it;
	// otherwise, the files are at the root and we use tmpDir directly.
	srcDir := tmpDir
	if _, err := os.Stat(filepath.Join(tmpDir, distDirName)); err == nil {
		srcDir = filepath.Join(tmpDir, distDirName)
	}

	// Atomic swap: rename source to final
	finalDir := filepath.Join(destDir, distDirName)
	oldDir := filepath.Join(destDir, distDirName+".old")
	// Remove previous backup if exists
	os.RemoveAll(oldDir)
	// Move current dist out of the way if it exists
	os.Rename(finalDir, oldDir)
	// Move new dist into place
	if err := os.Rename(srcDir, finalDir); err != nil {
		// Rollback
		os.RemoveAll(finalDir)
		os.Rename(oldDir, finalDir)
		return fmt.Errorf("rename new dist: %w", err)
	}
	os.RemoveAll(oldDir)

	return nil
}

func extractTarGz(r io.Reader, dest string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		// Normalize: strip leading ./ so "./dist" becomes "dist"
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "" || name == "." {
			continue // skip bare directory entry
		}

		target := filepath.Join(dest, name)

		// Security: prevent path traversal
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("path traversal detected: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}

// newHTTPClient creates an HTTP client that respects proxy configuration
func newHTTPClient() *http.Client {
	transport := &http.Transport{}
	if conf.Conf != nil && conf.Conf.ProxyAddress != "" {
		if proxyURL := mustParseURL(conf.Conf.ProxyAddress); proxyURL != nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Minute,
	}
}

func mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		utils.Log.Warnf("[frontend] invalid proxy URL %q: %v", raw, err)
		return nil
	}
	return u
}

// EnsureDist ensures a valid frontend dist is available, fetching if necessary.
// It first tries the dynamic dist, then falls back to fetching from GitHub.
// Returns the path to the dist directory, or empty string if no dist is available.
func EnsureDist(ctx context.Context) string {
	// If user explicitly configured dist_dir, use that
	if conf.Conf != nil && conf.Conf.DistDir != "" {
		if _, err := os.Stat(filepath.Join(conf.Conf.DistDir, "index.html")); err == nil {
			return conf.Conf.DistDir
		}
	}

	// Check if dynamic dist already exists
	if HasValidDist() {
		return filepath.Join(GetDistPath(), distDirName)
	}

	// If auto-fetch is enabled (and WebVersion is rolling/beta/dev), try fetching
	if shouldAutoFetch() {
		utils.Log.Infof("[frontend] no local dist found, fetching from rolling release...")
		result, err := FetchFromRolling(ctx)
		if err != nil {
			utils.Log.Warnf("[frontend] failed to fetch from rolling: %v", err)
			// Fall through to return empty (embedded dist will be used as fallback)
			return ""
		}
		return result.DistPath
	}

	return ""
}

func shouldAutoFetch() bool {
	v := conf.WebVersion
	return v == "" || v == "rolling" || v == "beta" || v == "dev"
}

// Ensure the directory exists for the frontend dist
func init() {
	_ = os.MkdirAll(GetDistPath(), 0755)
}

// Ensure that the sync.Once pattern is used for the fetcher
var (
	fetchMu     sync.Mutex
	fetchDone   bool
	fetchResult string
)

// EnsureDistOnce is a thread-safe version of EnsureDist that only fetches once per process.
// On failure, it does not lock the state so subsequent calls can retry.
func EnsureDistOnce(ctx context.Context) string {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	if fetchDone {
		return fetchResult
	}
	result := EnsureDist(ctx)
	if result != "" {
		fetchResult = result
		fetchDone = true
	}
	return result
}

// ResetFetchState resets the fetch state (used for testing or re-fetch)
func ResetFetchState() {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	fetchDone = false
	fetchResult = ""
}
