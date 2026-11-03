package frontend

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
)

// distEnv points the data dir at a temp dir, selects a channel the watcher
// follows, and restores everything afterwards.
func distEnv(t *testing.T) {
	t.Helper()
	oldData, oldConf, oldVer, oldFetch := flags.DataDir, conf.Conf, conf.WebVersion, fetchRolling
	flags.DataDir = t.TempDir()
	conf.Conf = &conf.Config{}
	conf.WebVersion = "rolling"
	ResetFetchState()
	t.Cleanup(func() {
		StopWatcher()
		flags.DataDir, conf.Conf, conf.WebVersion, fetchRolling = oldData, oldConf, oldVer, oldFetch
		ResetFetchState()
	})
}

// releaseServer serves a one-file frontend release and counts every request it gets.
func releaseServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	tarData := createTestTarGz(t, map[string]string{"./dist/index.html": "<html>fetched</html>"})
	var hits atomic.Int32
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case fmt.Sprintf("/repos/%s/releases/tags/rolling", getFrontendRepo()):
			_, _ = w.Write([]byte(fmt.Sprintf(`{"tag_name":"rolling","assets":[{"name":"openlist-frontend-dist.tar.gz","browser_download_url":"%s/download/dist.tar.gz"}]}`, ts.URL)))
		case fmt.Sprintf("/repos/%s/git/ref/tags/rolling", getFrontendRepo()):
			_, _ = w.Write([]byte(`{"object":{"type":"commit","sha":"distdirtest1234567890distdirtest1234567890"}}`))
		case "/download/dist.tar.gz":
			_, _ = w.Write(tarData)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

func useServer(ts *httptest.Server) {
	fetchRolling = func(ctx context.Context) (*FetchResult, error) { return fetchFromTag(ctx, "rolling", ts.URL) }
}

func TestEmptyDistDirKeepsAutoFetch(t *testing.T) {
	distEnv(t)
	ts, hits := releaseServer(t)
	useServer(ts)
	conf.Conf.DistDir = ""
	if OperatorManagedDist() || !shouldAutoFetch() {
		t.Fatal("empty dist_dir must not be operator-managed")
	}
	var updated atomic.Int32
	w := NewWatcher(func() { updated.Add(1) })
	w.check()
	if hits.Load() == 0 || updated.Load() != 1 {
		t.Fatalf("watcher with empty dist_dir: %d requests, %d reloads, want fetch and one reload", hits.Load(), updated.Load())
	}
}

// Deployments set dist_dir to the watcher's own directory; that must keep updating.
func TestDistDirEqualToTheWatcherDirKeepsAutoFetch(t *testing.T) {
	distEnv(t)
	own := filepath.Join(GetDistPath(), distDirName)
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	rel, relErr := filepath.Rel(cwd, own)
	forms := map[string]string{
		"absolute":       own,
		"trailing slash": own + string(filepath.Separator),
		"dot segments":   filepath.Join(own, "..", distDirName) + string(filepath.Separator) + ".",
		"double slash":   own + string(filepath.Separator) + string(filepath.Separator),
	}
	if relErr == nil {
		forms["relative"] = rel
	}
	for name, form := range forms {
		t.Run(name, func(t *testing.T) {
			conf.Conf.DistDir = form
			if OperatorManagedDist() {
				t.Fatalf("dist_dir %q is the watcher's own dir but counted as operator-managed", form)
			}
			if !shouldAutoFetch() {
				t.Fatalf("auto-fetch stopped for dist_dir %q", form)
			}
		})
	}
}

func TestCustomDistDirIsNeverFetchedIntoOrSwapped(t *testing.T) {
	distEnv(t)
	ts, hits := releaseServer(t)
	useServer(ts)
	custom := t.TempDir()
	index := filepath.Join(custom, "index.html")
	if err := os.WriteFile(index, []byte("<html>operator</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	conf.Conf.DistDir = custom
	if !OperatorManagedDist() || shouldAutoFetch() {
		t.Fatal("a custom dist_dir must be operator-managed and stop auto-fetch")
	}

	var updated atomic.Int32
	NewWatcher(func() { updated.Add(1) }).check()
	StartWatcher(func() { updated.Add(1) })
	if globalWatcher != nil {
		t.Fatal("the watcher was started for an operator-managed dist_dir")
	}
	if EnsureDist(context.Background()) != custom {
		t.Fatal("EnsureDist must return the operator's directory")
	}
	if hits.Load() != 0 || updated.Load() != 0 {
		t.Fatalf("release server got %d requests and %d reloads, want 0/0", hits.Load(), updated.Load())
	}
	if _, err := os.Stat(filepath.Join(GetDistPath(), distDirName)); err == nil {
		t.Fatal("a dist was fetched next to the operator's directory")
	}
	if b, _ := os.ReadFile(index); string(b) != "<html>operator</html>" {
		t.Fatalf("operator's index.html was touched: %q", b)
	}

	// even with no index.html in the custom dir there is no fetch
	_ = os.Remove(index)
	if got := EnsureDist(context.Background()); got != "" || hits.Load() != 0 {
		t.Fatalf("EnsureDist fetched for a custom dist_dir: %q, %d requests", got, hits.Load())
	}
}

// frontend_source embedded: the node serves what is built into the binary. The
// watcher does not start, nothing is fetched, and a check does not reach the
// release server even when an older fetched copy is on disk.
func TestEmbeddedSourceNeverStartsTheWatcherOrFetches(t *testing.T) {
	distEnv(t)
	ts, hits := releaseServer(t)
	useServer(ts)
	conf.Conf.FrontendSource = conf.FrontendSourceEmbedded
	if !EmbeddedOnly() {
		t.Fatal("embedded source not recognised")
	}
	var updated atomic.Int32
	NewWatcher(func() { updated.Add(1) }).check()
	StartWatcher(func() { updated.Add(1) })
	if globalWatcher != nil {
		t.Fatal("the watcher was started")
	}
	if got := EnsureDist(context.Background()); got != "" {
		t.Fatalf("EnsureDist = %q, want nothing", got)
	}
	if _, err := FetchFromRolling(context.Background()); err == nil {
		t.Fatal("FetchFromRolling worked in embedded mode")
	}
	if hits.Load() != 0 || updated.Load() != 0 {
		t.Fatalf("release server got %d requests, %d reloads, want 0/0", hits.Load(), updated.Load())
	}
	if _, err := os.Stat(filepath.Join(GetDistPath(), distDirName)); err == nil {
		t.Fatal("a dist was fetched")
	}
}
