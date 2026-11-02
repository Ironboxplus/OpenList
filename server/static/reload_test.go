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
