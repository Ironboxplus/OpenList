# Frontend — Dist Serving, Movi-Player Integration, Subtitle Handling

Deep-dive reference for the OpenList frontend distribution pipeline and the movi-player video subsystem. See [INDEX.MD](../../INDEX.MD) for the subsystem map.

---

## Frontend Dist Serving

**Backend location**: `server/static/static.go`, `internal/frontend/`

**Stack**: SolidJS + Vite + Hope UI. Builds to `dist/` which gets embedded into the backend binary via `go:embed`.

The frontend dist has two sources, with a strict priority:

1. **Embedded dist** (`public/dist/` via `go:embed`): Baked into the binary at build time. Always used on startup.
2. **Dynamic dist** (fetched by watcher): The `frontend.Watcher` checks GitHub every 30 minutes for a newer rolling release. If found, it downloads to `data/frontend_dist/` and hot-swaps the serving FS via `ReloadStatic()`.

**Key design rule**: `initStatic()` always starts with the embedded dist (or `dist_dir` if configured). The cached dynamic dist in the data volume is **never** read on startup — only the watcher can activate it after verifying a newer version exists on GitHub. This prevents stale cache from overriding a newer Docker image.

**Configuration** (`config.json`):
- `dist_dir`: Override with a custom local directory (highest priority, skips embedded)
- `frontend_repo`: GitHub repo for the watcher to check (default: `Ironboxplus/OpenList-Frontend`)

**Startup flow**:
```
initStatic()  →  embedded dist (or dist_dir)
     ↓
StartWatcher(ReloadStatic)  →  background goroutine
     ↓  (every 30min)
FetchFromRolling()  →  compare cache version vs GitHub rolling tag commit
     ↓  (if newer)
downloadAndExtract()  →  atomic swap in data/frontend_dist/dist/
     ↓
ReloadStatic()  →  swap staticFS to new dist, re-render index.html
```

---

## Frontend Build Commands

```bash
# From OpenList-Frontend/
pnpm install                    # Install dependencies
pnpm dev                        # Dev server (port 5173)
pnpm build                      # Production build
pnpm test                       # Run vitest tests
```

---

## Preview System

Previews registered in `src/pages/home/previews/index.ts`. Each preview declares `prior: true/false` for priority ordering.

Current video priority: **movi-player** (default) > **Artplayer** (fallback).

---

## Movi-Player Integration

**npm package**: `movi-player ^0.3.2`  
**Local reference clone**: `movi-player/` (clean upstream mirror, no local changes)

movi-player (FFmpeg WASM + WebCodecs) is the default video player. It requires COOP/COEP headers for SharedArrayBuffer — set in backend `server/router.go`:
- `Cross-Origin-Opener-Policy: same-origin`
- `Cross-Origin-Embedder-Policy: credentialless` (chosen over `require-corp` to avoid breaking CDN resources)

**Subtitle support**:
| Format | Handling |
|--------|---------|
| SRT/VTT (external) | movi-player native parsing |
| ASS (external) | JASSUB (libass WASM) overlay canvas rendering with font fallback |
| PGS/SUP (external) | libpgs overlay canvas rendering |
| All embedded formats | movi-player WASM demuxer |

**Subtitle manager** (`subtitle-manager.ts`): orchestrates overlay canvases for ASS and PGS formats that movi-player cannot render natively.

---

## Movi-Player npm Package Version

- Current: **0.3.2** (referenced via `^0.3.2` in package.json)
- Key additions in 0.3.0+: TrueHD/MLP software audio decoding, HDR/DoVi hardware-decode retention, open-GOP HEVC fallback, multichannel passthrough

**Known Limitations**:
| Limitation | Reason |
|-----------|--------|
| External ASS/PGS not rendered natively | Handled by JASSUB (ASS) and libpgs (PGS) overlay canvases |
| Dolby Vision: purple tint | WASM decoder lacks DV enhancement layer |
| Requires COOP/COEP headers | SharedArrayBuffer for WASM threads |

---

## 115 Official Play Source (`115_video.tsx`)

Frontend component that renders an Artplayer with quality selector for the `/api/fs/video_play` endpoint. Lists available resolutions sorted high→low. See [new-apis-2026-06-21.md](new-apis-2026-06-21.md) for the backend API details.

---

## Other Frontend Subsystems (vs Upstream)

| Feature | Key files |
|---------|-----------|
| VideoTreeList | `src/pages/home/previews/video/VideoTreeList.tsx` — tree-structured video browser replacing flat dropdown |
| Mobile toolbar | `src/utils/touch.ts` (triple-detection), `Icon.tsx` (icon+label on touch) |
| Header UserMenu | `user-menu.ts` pure-function entries; `UserMenu.tsx` avatar dropdown; Footer login removed |
| Motion presets | `src/utils/motion-presets.ts` — stagger/fade/scale entry animations |
| Persisted state | `src/utils/persisted.ts` — `createPersistedSignal`; storages driver filter persists across navigation |
| ECharts component | `src/components/EChart.tsx` — tree-shaken PieChart+BarChart, colorMode-aware |
| GridItem overflow | Respects `list_item_filename_overflow` (multi_line / scrollable / ellipsis), consistent with ListItem |
