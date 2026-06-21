package plugin

import (
	"fmt"
	"reflect"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// Runtime is a yaegi-backed host for Go-source plugins. Plugins are plain `.go`
// files interpreted in-process — no compile step, no WASM toolchain. Reloading a
// plugin under a name that already exists atomically replaces it (hot-reload).
//
// Coupling boundary: interpreted plugins may import ONLY this package (for the
// API/HookContext types) plus the Go standard library. The host's internal/*
// packages are never registered with the interpreter, so a plugin literally
// cannot reach into backend internals — keeping plugins decoupled from churn.
//
// Trust model: yaegi blocks `unsafe`, `os/exec`, fork and `os.Exit` by default,
// but it is NOT a hostile-code sandbox. Plugins are admin-installed, trusted code.
type Runtime struct {
	mu      sync.Mutex
	reg     *Registry
	plugins map[string]*loadedPlugin
}

// loadedPlugin tracks a live interpreter and its optional teardown callback.
type loadedPlugin struct {
	interp   *interp.Interpreter
	onUnload func()
}

// API is the surface a backend plugin uses to talk to the host. It is the single
// coupling point between plugins and the server — deliberately small and stable.
type API interface {
	// Name returns the plugin's name (its file name without extension).
	Name() string
	// Subscribe registers handler for a named hook. Lower order runs first.
	// Subscribe to the well-known Hook* constants exported by this package.
	Subscribe(hook string, order int, handler Handler)
	// Log writes a line to the server log, tagged with the plugin's name.
	Log(args ...any)
	// Logf is the printf-style variant of Log.
	Logf(format string, args ...any)
	// GetSetting reads a server setting by key ("" if unset/unavailable).
	GetSetting(key string) string
	// SetSetting persists a server setting. Use a plugin-namespaced key.
	SetSetting(key, value string) error
}

// pluginAPI is the per-plugin API implementation, pre-bound to the plugin's name
// and the shared registry so Subscribe attributes hooks to the right plugin
// (which is what makes UnsubscribeAll-on-reload correct).
type pluginAPI struct {
	name string
	reg  *Registry
}

func (a *pluginAPI) Name() string { return a.name }

func (a *pluginAPI) Subscribe(hook string, order int, handler Handler) {
	a.reg.Subscribe(a.name, hook, order, handler)
}

func (a *pluginAPI) Log(args ...any) {
	log.Infof("[go-plugin %s] %s", a.name, fmt.Sprint(args...))
}

func (a *pluginAPI) Logf(format string, args ...any) {
	log.Infof("[go-plugin %s] %s", a.name, fmt.Sprintf(format, args...))
}

func (a *pluginAPI) GetSetting(key string) string {
	if SettingGetter == nil {
		return ""
	}
	return SettingGetter(key)
}

func (a *pluginAPI) SetSetting(key, value string) error {
	if SettingSetter == nil {
		return fmt.Errorf("setting persistence is not available")
	}
	return SettingSetter(key, value)
}

// hostSymbols exposes this package's plugin-facing types to interpreted plugins
// under its real import path, so a plugin can:
//
//	import plugin "github.com/OpenListTeam/OpenList/v4/internal/plugin"
//
// Only API, Handler and HookContext are exported — nothing else of the host.
var hostSymbols = interp.Exports{
	"github.com/OpenListTeam/OpenList/v4/internal/plugin/plugin": map[string]reflect.Value{
		"API":         reflect.ValueOf((*API)(nil)),
		"Handler":     reflect.ValueOf((*Handler)(nil)),
		"HookContext": reflect.ValueOf((*HookContext)(nil)),
		// Well-known hook names, so plugins can use the constants instead of
		// hard-coding strings.
		"HookStorageCreated": reflect.ValueOf(HookStorageCreated),
		"HookStorageUpdated": reflect.ValueOf(HookStorageUpdated),
		"HookStorageDeleted": reflect.ValueOf(HookStorageDeleted),
		"HookFsObjsUpdated":  reflect.ValueOf(HookFsObjsUpdated),
		"HookFsListAfter":    reflect.ValueOf(HookFsListAfter),
		"HookFsLinkAfter":    reflect.ValueOf(HookFsLinkAfter),
		"HookUserLoginAfter": reflect.ValueOf(HookUserLoginAfter),
	},
}

// NewRuntime builds a runtime that subscribes plugins into reg.
func NewRuntime(reg *Registry) *Runtime {
	return &Runtime{reg: reg, plugins: map[string]*loadedPlugin{}}
}

// Load interprets a Go-source plugin under name and runs its entry point. The
// plugin must declare `package main` and export `func OnLoad(api plugin.API)`.
// It may optionally export `func OnUnload()`, called before the plugin is
// dropped or hot-reloaded. If a plugin with the same name is already loaded its
// teardown runs and its hooks are dropped first, so Load doubles as hot-reload.
func (r *Runtime) Load(name string, source []byte) error {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		return fmt.Errorf("use stdlib: %w", err)
	}
	if err := i.Use(hostSymbols); err != nil {
		return fmt.Errorf("use host symbols: %w", err)
	}
	if _, err := i.Eval(string(source)); err != nil {
		return fmt.Errorf("eval %q: %w", name, err)
	}
	v, err := i.Eval("main.OnLoad")
	if err != nil {
		return fmt.Errorf("plugin %q must export func OnLoad(plugin.API): %w", name, err)
	}
	onLoad, ok := v.Interface().(func(API))
	if !ok {
		return fmt.Errorf("plugin %q OnLoad has wrong signature, want func(plugin.API)", name)
	}
	// Optional teardown hook.
	var onUnload func()
	if uv, uerr := i.Eval("main.OnUnload"); uerr == nil {
		onUnload, _ = uv.Interface().(func())
	}

	// Tear down any previous version before the new one takes over.
	r.teardown(name)

	r.mu.Lock()
	r.plugins[name] = &loadedPlugin{interp: i, onUnload: onUnload}
	r.mu.Unlock()

	onLoad(&pluginAPI{name: name, reg: r.reg})
	return nil
}

// teardown runs a plugin's OnUnload (if any), drops its hook subscriptions, and
// removes it from the live set. Safe to call for an unknown name.
func (r *Runtime) teardown(name string) {
	r.mu.Lock()
	lp, ok := r.plugins[name]
	delete(r.plugins, name)
	r.mu.Unlock()
	if !ok {
		return
	}
	if lp.onUnload != nil {
		func() {
			// A misbehaving teardown must not take down the server.
			defer func() {
				if rec := recover(); rec != nil {
					log.Warnf("[plugin] %q OnUnload panicked: %v", name, rec)
				}
			}()
			lp.onUnload()
		}()
	}
	r.reg.UnsubscribeAll(name)
}

// Unload removes a plugin: runs its teardown and drops all of its hooks.
func (r *Runtime) Unload(name string) { r.teardown(name) }

// Loaded reports whether a plugin is currently loaded.
func (r *Runtime) Loaded(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.plugins[name]
	return ok
}

// Close tears down all loaded plugins.
func (r *Runtime) Close() error {
	r.mu.Lock()
	names := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		names = append(names, name)
	}
	r.mu.Unlock()
	for _, name := range names {
		r.teardown(name)
	}
	return nil
}
