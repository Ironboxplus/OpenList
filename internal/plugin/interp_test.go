package plugin

import "testing"

// A minimal, valid plugin: subscribes to a hook and mutates the payload.
const demoPlugin = `
package main

import plug "github.com/OpenListTeam/OpenList/v4/internal/plugin"

func OnLoad(api plug.API) {
	api.Log("demo loaded")
	api.Subscribe("demo", 0, func(c *plug.HookContext) error {
		c.Payload["touched"] = "v1"
		return nil
	})
}
`

// Same hook, different payload value — used to verify hot-reload replaces the
// old handler instead of stacking a second one.
const demoPluginV2 = `
package main

import plug "github.com/OpenListTeam/OpenList/v4/internal/plugin"

func OnLoad(api plug.API) {
	api.Subscribe("demo", 0, func(c *plug.HookContext) error {
		c.Payload["touched"] = "v2"
		return nil
	})
}
`

func TestRuntimeLoadAndFire(t *testing.T) {
	reg := NewRegistry()
	rt := NewRuntime(reg)
	defer rt.Close()

	if err := rt.Load("demo", []byte(demoPlugin)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !rt.Loaded("demo") {
		t.Fatal("expected demo loaded")
	}
	ctx, errs := reg.Fire("demo", map[string]any{})
	if len(errs) != 0 {
		t.Fatalf("unexpected handler errors: %v", errs)
	}
	if ctx.Payload["touched"] != "v1" {
		t.Fatalf("expected payload touched=v1, got %v", ctx.Payload["touched"])
	}
}

func TestRuntimeHotReloadReplacesHandlers(t *testing.T) {
	reg := NewRegistry()
	rt := NewRuntime(reg)
	defer rt.Close()

	if err := rt.Load("demo", []byte(demoPlugin)); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if err := rt.Load("demo", []byte(demoPluginV2)); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Exactly one handler must remain, producing v2 (not v1, and not both).
	if got := len(reg.Handlers("demo")); got != 1 {
		t.Fatalf("expected 1 handler after reload, got %d", got)
	}
	ctx, _ := reg.Fire("demo", map[string]any{})
	if ctx.Payload["touched"] != "v2" {
		t.Fatalf("expected v2 after reload, got %v", ctx.Payload["touched"])
	}
}

func TestRuntimeUnloadDropsHooks(t *testing.T) {
	reg := NewRegistry()
	rt := NewRuntime(reg)
	defer rt.Close()

	_ = rt.Load("demo", []byte(demoPlugin))
	rt.Unload("demo")
	if rt.Loaded("demo") {
		t.Fatal("expected demo unloaded")
	}
	if got := len(reg.Handlers("demo")); got != 0 {
		t.Fatalf("expected 0 handlers after unload, got %d", got)
	}
}

func TestRuntimeBadSourceErrors(t *testing.T) {
	reg := NewRegistry()
	rt := NewRuntime(reg)
	defer rt.Close()

	if err := rt.Load("bad", []byte("package main\nthis is not go")); err == nil {
		t.Fatal("expected error for invalid plugin source")
	}
	if rt.Loaded("bad") {
		t.Fatal("a plugin that failed to eval must not be considered loaded")
	}
}

// Verifies the exported Hook* constants are usable from interpreted plugins.
func TestRuntimeHookConstantsExposed(t *testing.T) {
	reg := NewRegistry()
	rt := NewRuntime(reg)
	defer rt.Close()

	src := `
package main

import plug "github.com/OpenListTeam/OpenList/v4/internal/plugin"

func OnLoad(api plug.API) {
	api.Subscribe(plug.HookFsListAfter, 0, func(c *plug.HookContext) error {
		c.Payload["seen"] = true
		return nil
	})
}
`
	if err := rt.Load("c", []byte(src)); err != nil {
		t.Fatalf("Load using Hook constant: %v", err)
	}
	ctx, _ := reg.Fire(HookFsListAfter, map[string]any{})
	if ctx.Payload["seen"] != true {
		t.Fatal("expected handler subscribed via Hook constant to fire")
	}
}

func TestRuntimeMissingOnLoadErrors(t *testing.T) {
	reg := NewRegistry()
	rt := NewRuntime(reg)
	defer rt.Close()

	src := "package main\nvar X = 1\n"
	if err := rt.Load("noentry", []byte(src)); err == nil {
		t.Fatal("expected error when plugin has no OnLoad")
	}
}
