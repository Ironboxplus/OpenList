package plugin

import "testing"

func findInfo(infos []PluginInfo, name string) (PluginInfo, bool) {
	for _, i := range infos {
		if i.Name == name {
			return i, true
		}
	}
	return PluginInfo{}, false
}

func TestSaveListAndLoad(t *testing.T) {
	m := newTestManager(t)
	if err := m.SaveGoPlugin("demo", demoPlugin); err != nil {
		t.Fatalf("SaveGoPlugin: %v", err)
	}
	info, ok := findInfo(m.ListGoPlugins(), "demo")
	if !ok {
		t.Fatal("expected demo in plugin list")
	}
	if !info.Enabled || !info.Loaded || info.Error != "" {
		t.Fatalf("expected demo enabled+loaded+no-error, got %+v", info)
	}
	// Source round-trips.
	src, err := m.GetGoPluginSource("demo")
	if err != nil || src != demoPlugin {
		t.Fatalf("GetGoPluginSource mismatch: err=%v", err)
	}
}

func TestSaveInvalidName(t *testing.T) {
	m := newTestManager(t)
	if err := m.SaveGoPlugin("../evil", demoPlugin); err == nil {
		t.Fatal("expected invalid name to be rejected")
	}
}

func TestSaveBrokenSourceReportsError(t *testing.T) {
	m := newTestManager(t)
	// Write succeeds, but the code fails to load → recorded as an error, not loaded.
	if err := m.SaveGoPlugin("broken", "package main\nnot valid go"); err != nil {
		t.Fatalf("SaveGoPlugin (write) should succeed: %v", err)
	}
	info, ok := findInfo(m.ListGoPlugins(), "broken")
	if !ok {
		t.Fatal("expected broken plugin listed")
	}
	if info.Loaded {
		t.Fatal("broken plugin must not be loaded")
	}
	if info.Error == "" {
		t.Fatal("expected a load error to be reported")
	}
}

func TestEnableDisable(t *testing.T) {
	m := newTestManager(t)
	if err := m.SaveGoPlugin("demo", demoPlugin); err != nil {
		t.Fatal(err)
	}
	if err := m.SetGoPluginEnabled("demo", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if m.runtime.Loaded("demo") {
		t.Fatal("expected demo unloaded after disable")
	}
	info, _ := findInfo(m.ListGoPlugins(), "demo")
	if info.Enabled {
		t.Fatal("expected demo to be marked disabled")
	}
	if err := m.SetGoPluginEnabled("demo", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !m.runtime.Loaded("demo") {
		t.Fatal("expected demo loaded after re-enable")
	}
}

func TestDeleteUnloads(t *testing.T) {
	m := newTestManager(t)
	if err := m.SaveGoPlugin("demo", demoPlugin); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteGoPlugin("demo"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if m.runtime.Loaded("demo") {
		t.Fatal("expected demo unloaded after delete")
	}
	if _, ok := findInfo(m.ListGoPlugins(), "demo"); ok {
		t.Fatal("expected demo gone from list")
	}
}

func TestGetSettingCapability(t *testing.T) {
	// The API exposes injected capabilities; verify the nil-safe defaults and a
	// wired getter.
	api := &pluginAPI{name: "x", reg: NewRegistry()}
	SettingGetter = nil
	if api.GetSetting("k") != "" {
		t.Fatal("expected empty string when no getter is wired")
	}
	SettingGetter = func(key string) string {
		if key == "k" {
			return "v"
		}
		return ""
	}
	defer func() { SettingGetter = nil }()
	if api.GetSetting("k") != "v" {
		t.Fatal("expected wired getter to return v")
	}
}
