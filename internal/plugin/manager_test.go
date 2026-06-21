package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeGo(t *testing.T, dir, name, source string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(context.Background(), t.TempDir(), "/api/plugin/asset")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestManagerSyncLoadsGo(t *testing.T) {
	m := newTestManager(t)
	writeGo(t, m.goDir(), "demo.go", demoPlugin)

	changed, err := m.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(changed) != 1 || changed[0] != "demo" {
		t.Fatalf("expected [demo] loaded, got %v", changed)
	}
	if !m.runtime.Loaded("demo") {
		t.Fatal("expected demo loaded in runtime")
	}
	// The loaded plugin's hook must actually fire through the manager's registry.
	ctx, errs := m.registry.Fire("demo", map[string]any{})
	if len(errs) != 0 || ctx.Payload["touched"] != "v1" {
		t.Fatalf("expected touched=v1, got %v errs=%v", ctx.Payload["touched"], errs)
	}
}

func TestManagerSyncMissingDirIsEmpty(t *testing.T) {
	m := newTestManager(t)
	changed, err := m.Sync()
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("expected no changes, got %v", changed)
	}
}

func TestManagerSyncUnchangedNotReloaded(t *testing.T) {
	m := newTestManager(t)
	writeGo(t, m.goDir(), "demo.go", demoPlugin)
	if _, err := m.Sync(); err != nil {
		t.Fatal(err)
	}
	changed, err := m.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("expected no reload on unchanged file, got %v", changed)
	}
}

func TestManagerSyncUnloadsDeleted(t *testing.T) {
	m := newTestManager(t)
	path := writeGo(t, m.goDir(), "demo.go", demoPlugin)
	if _, err := m.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Sync(); err != nil {
		t.Fatal(err)
	}
	if m.runtime.Loaded("demo") {
		t.Fatal("expected demo unloaded after file removed")
	}
	if got := len(m.registry.Handlers("demo")); got != 0 {
		t.Fatalf("expected hooks dropped after unload, got %d", got)
	}
}

func TestManagerSyncReloadsChanged(t *testing.T) {
	m := newTestManager(t)
	path := writeGo(t, m.goDir(), "demo.go", demoPlugin)
	if _, err := m.Sync(); err != nil {
		t.Fatal(err)
	}
	// Bump mtime forward so the change is detected deterministically.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	changed, err := m.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != "demo" {
		t.Fatalf("expected demo reloaded, got %v", changed)
	}
}

func TestManagerFrontendManifest(t *testing.T) {
	m := newTestManager(t)
	dir := m.frontendDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello.js"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	mf := m.FrontendManifest()
	if len(mf.Plugins) != 1 {
		t.Fatalf("expected 1 manifest entry, got %d", len(mf.Plugins))
	}
	if mf.Plugins[0].ID != "hello" {
		t.Fatalf("expected id hello, got %s", mf.Plugins[0].ID)
	}
	if mf.Plugins[0].URL != "/api/plugin/asset/hello.js" {
		t.Fatalf("unexpected url %s", mf.Plugins[0].URL)
	}
}

func TestManagerFrontendManifestEmptyWhenNoDir(t *testing.T) {
	m := newTestManager(t)
	mf := m.FrontendManifest()
	if mf.Plugins == nil || len(mf.Plugins) != 0 {
		t.Fatalf("expected empty (non-nil) manifest, got %v", mf.Plugins)
	}
}

func TestManagerAssetPathRejectsTraversal(t *testing.T) {
	m := newTestManager(t)
	if _, ok := m.AssetPath("../../etc/passwd"); ok {
		t.Fatal("expected traversal to be rejected")
	}
	if _, ok := m.AssetPath("ok.js"); !ok {
		t.Fatal("expected normal filename to be accepted")
	}
}
