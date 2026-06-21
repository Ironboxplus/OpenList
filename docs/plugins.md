# Plugin System — Frontend Registry/Slots + Backend yaegi Runtime

Two independent, hot-reloadable plugin planes. The **frontend** plane renders UI
via SolidJS slots; the **backend** plane runs admin-authored Go source in-process
via the [yaegi](https://github.com/traefik/yaegi) interpreter. See
[INDEX.MD](../../INDEX.MD) for the subsystem map.

---

## Frontend Plugin System (`OpenList-Frontend/src/plugins/`)

| File | Role |
|------|------|
| `registry.ts` | Pure TS registry. `register(plugin)` / `unregister(id)` / `selectSlot(name)`. Enable/disable persisted in `localStorage`. Same `id` hot-replaces. |
| `PluginSlot.tsx` | Reactive slot renderer. `<PluginSlot name="header-right"/>` renders all enabled plugins for that slot. |
| `loader.ts` | `loadExternalPlugins(manifest, importer?)` dynamically imports JS assets listed in the backend manifest. |
| `index.ts` | `installBuiltinPlugins()` + `installExternalPlugins()` (fetches `GET /api/plugin/manifest`). |

**Built-in plugins**: `disk-usage` (header-right; reads `objStore.mountDetails`, updated on every navigation), `favorites` (header-right), `stats` (manage-dashboard ECharts), `media-preview` (behavior-only hover preview). Slot names in use: `header-right`, `manage-dashboard`.

---

## Backend Plugin System (`OP/internal/plugin/`)

Plugins are plain **`.go` source files** interpreted by yaegi — no compile step,
no WASM toolchain. (Previously this was a wazero/WASM runtime; swapped to yaegi
2026-06-21 so users can author plugins in plain Go.)

| File | Role |
|------|------|
| `registry.go` | Hook event bus. `Subscribe(pluginID, hook, order, handler)` / `Fire(hook, payload)` / `UnsubscribeAll(pluginID)` / `HasHandlers(hook)`. One handler error never blocks others; `HookContext.Payload` is mutable across the chain. |
| `interp.go` | yaegi `Runtime`: `Load(name, source)` / `Unload` / `Loaded` / `Close`. One interpreter per plugin; same-name `Load` hot-reloads. Exposes `stdlib.Symbols` + a curated `hostSymbols` (this package's `API`, `Handler`, `HookContext`, and `Hook*` constants) — **nothing else of the host**, so plugins cannot import `internal/*`. Defines the `API` interface and per-plugin `pluginAPI`. |
| `hooks.go` | Well-known `Hook*` constants, the `FireHook`/`HasSubscribers` helpers, and injectable capability vars (`SettingGetter`/`SettingSetter`) — kept here so the plugin package never imports `internal/op` (which fires hooks → would be an import cycle). |
| `manager.go` | Watches `<data>/plugins/go/*.go` (mtime+size change detection) and `<data>/plugins/frontend/*.js`. `Start(interval)` polling goroutine; `Sync()` reconciles; tracks per-plugin load errors. `AssetPath` has traversal protection. |
| `manage.go` | User-plugin management: `ListGoPlugins` / `GetGoPluginSource` / `SaveGoPlugin` / `DeleteGoPlugin` / `SetGoPluginEnabled`. Enable/disable toggles the file between `<name>.go` and `<name>.go.disabled`. Name charset is restricted (traversal-proof). |
| `internal/bootstrap/plugin.go` | `InitPlugins()`: wires `SettingGetter/Setter` (via op), bridges `op.RegisterStorageHook` + `op.RegisterObjsUpdateHook` into the plugin registry, starts the watcher. |

### Plugin contract

A backend plugin is a single `.go` file:

```go
package main

import plugin "github.com/OpenListTeam/OpenList/v4/internal/plugin"

// Required. Runs once on (re)load.
func OnLoad(api plugin.API) {
	api.Log("hello")
	api.Subscribe(plugin.HookFsListAfter, 0, func(c *plugin.HookContext) error {
		api.Logf("listed %v (%v items)", c.Payload["path"], c.Payload["count"])
		return nil
	})
}

// Optional. Runs before the plugin is removed or hot-reloaded.
func OnUnload() {}
```

`plugin.API` surface: `Name()`, `Subscribe(hook, order, handler)`, `Log(...)`,
`Logf(fmt, ...)`, `GetSetting(key)`, `SetSetting(key, value)`. Plugins may also
import the Go standard library (yaegi blocks `unsafe`, `os/exec`, fork, `os.Exit`
by default). **Trust model**: plugins are admin-installed, trusted code — yaegi is
defense-in-depth, not a hostile-code sandbox.

### Hook points (fired by the host)

| Constant | Fired at | Payload |
|----------|----------|---------|
| `HookStorageCreated` / `Updated` / `Deleted` | storage lifecycle (bridged from `op` StorageHook) | `mount_path`, `driver` |
| `HookFsObjsUpdated` | dir contents change (bridged from `op` ObjsUpdateHook) | `path`, `count` |
| `HookFsListAfter` | after a directory is listed (`handles.FsList`) | `path`, `count` |
| `HookFsLinkAfter` | after a link is generated (`handles.Link`) | `path` |
| `HookUserLoginAfter` | after successful login (`handles.loginHash`) | `username` |

`FireHook` is a no-op when no plugin subscribes, so hot paths gate payload
construction with `HasSubscribers(hook)`.

### API endpoints

| Route | Auth | Purpose |
|-------|------|---------|
| `GET /api/plugin/manifest` | Public | Frontend JS asset manifest |
| `GET /api/plugin/asset/:name` | Public | Serve a frontend JS asset (traversal-guarded) |
| `GET /api/admin/plugin/list` | Admin | Backend Go plugins (+ load status/errors) and frontend plugins |
| `GET /api/admin/plugin/get?name=` | Admin | A backend plugin's source |
| `POST /api/admin/plugin/save` | Admin | Create/overwrite a backend plugin (`{name, source}`), loads immediately |
| `POST /api/admin/plugin/delete` | Admin | Delete a backend plugin (`{name}`) |
| `POST /api/admin/plugin/enable` | Admin | Enable/disable (`{name, enabled}`) |

The management UI is `OpenList-Frontend/src/pages/manage/plugins/Plugins.tsx`
(backend editor + frontend toggles), with the API client in `plugins/api.ts`.

### Dependency

`github.com/traefik/yaegi v0.16.1` — builds on the Go 1.25 toolchain; its
interpreter accepts up to **Go 1.22** language level, so plugin source must avoid
1.23+ features (iterators, generic type aliases). Replaces the former
`github.com/tetratelabs/wazero`.
