package static

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/frontend"
)

// ReloadStatic runs after the watcher fetched a frontend. With a custom
// dist_dir it must leave the operator's directory being served.
func TestReloadStaticKeepsACustomDistDir(t *testing.T) {
	oldData, oldConf := flags.DataDir, conf.Conf
	t.Cleanup(func() { flags.DataDir, conf.Conf = oldData, oldConf })
	flags.DataDir = t.TempDir()

	fetched := filepath.Join(frontend.GetDistPath(), "dist")
	custom := t.TempDir()
	for dir, body := range map[string]string{fetched: "fetched", custom: "operator"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	conf.Conf = &conf.Config{DistDir: custom}
	staticFS.swap(os.DirFS(custom))

	ReloadStatic()

	f, err := staticFS.Open("index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if b, _ := io.ReadAll(f); string(b) != "operator" {
		t.Fatalf("ReloadStatic replaced the operator's dist_dir with %q", b)
	}
}

// With frontend_source embedded the node serves the build in the binary: an
// older fetched copy on disk is not served at start and a reload does not swap
// to it.
func TestEmbeddedSourceServesTheEmbeddedDistNotAFetchedOne(t *testing.T) {
	oldData, oldConf := flags.DataDir, conf.Conf
	t.Cleanup(func() { flags.DataDir, conf.Conf = oldData, oldConf })
	flags.DataDir = t.TempDir()
	fetched := filepath.Join(frontend.GetDistPath(), "dist")
	if err := os.MkdirAll(fetched, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fetched, "index.html"), []byte("OLD FETCHED COPY"), 0o644); err != nil {
		t.Fatal(err)
	}
	index := func() string {
		f, err := staticFS.Open("index.html")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		return string(b)
	}

	conf.Conf = &conf.Config{FrontendSource: conf.FrontendSourceEmbedded}
	initStatic()
	embedded := index()
	if embedded == "OLD FETCHED COPY" {
		t.Fatal("the fetched copy is what initStatic serves")
	}
	ReloadStatic() // what the watcher would call after a fetch
	if got := index(); got != embedded {
		t.Fatalf("embedded node serves %q after a reload", got)
	}
}
