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

## Serving Priority and the In-Memory index.html Gotcha

There are three sources of truth for the frontend, in strict priority order:

| Priority    | Source                                            | When active                                                        |
|-------------|---------------------------------------------------|--------------------------------------------------------------------|
| 1 (highest) | `dist_dir` (config.json)                          | Set explicitly; `initStatic()` calls `os.DirFS(conf.Conf.DistDir)` |
| 2           | Watcher-fetched dist (`data/frontend_dist/dist`)  | `shouldAutoFetch()` true and a newer rolling release was found     |
| 3 (lowest)  | Embedded dist (`public/dist/` via `go:embed`)     | Default fallback when neither of the above applies                 |

**Critical implementation detail — index.html lives in memory, not on disk.**

`initIndex()` reads `index.html` from `staticFS` (or the CDN) once and stores it in `conf.RawIndexHtml`. `UpdateIndex()` derives `conf.ManageHtml` (for `/@manage/*`) and `conf.IndexHtml` (all other routes) from that raw copy. The SPA catch-all handler writes the in-memory string directly to the response — it never opens the file on disk per-request.

Consequence: **replacing files on disk does not affect what the server returns for `index.html`** until one of the following happens:

- The process restarts (which calls `initStatic()` / `initIndex()` again), OR
- The watcher downloads a genuinely newer rolling release and calls `ReloadStatic()`.

Meanwhile, static assets (`/assets/`, `/images/`, `/streamer/`, `/static/`) **do** serve live from `staticFS` via `http.FS`. So new asset chunks load immediately after you copy files to disk — but the old `index.html` still references the old chunk hashes, causing a mix of old UI and new files.

**Symptom pattern**: new asset requests return 200; the UI loads stale chunks; the browser console shows hash-mismatch errors or the manage page looks like a previous version.

`ReloadStatic()` (called by the watcher) fixes both: it calls `staticFS.swap(os.DirFS(distPath))` to update the asset FS, then re-runs `initIndex()` to reload index.html into memory.

---

## Deterministic Deploy Procedure (dist_dir method)

This is the recommended deployment path when you need a specific frontend build on disk — especially on hosts that cannot reach GitHub (e.g., hosts behind a firewall or with restricted egress).

```
# 1. Build locally
cd OpenList-Frontend/
pnpm install && pnpm build
# Output: dist/

# 2. Copy to host, atomically swap
# NEVER delete frontend_dist/ itself — the watcher owns that directory.
# Back up the old dist, then rename the new one into place.
REMOTE_DATA=/root/docker/opdata          # host-side path; maps to /opt/openlist/data inside container
CONTAINER_DATA=/opt/openlist/data

scp -r dist/ user@cfscan:${REMOTE_DATA}/frontend_dist/dist.new
ssh user@cfscan "
  cd ${REMOTE_DATA}/frontend_dist
  mv dist dist.bak.\$(date +%s)          # keep old as backup, never delete it
  mv dist.new dist
"

# 3. Set dist_dir in config.json (container path)
# In config.json on the host, set:
#   "dist_dir": "/opt/openlist/data/frontend_dist/dist"
# (the container-internal path, not the host path)

# 4. Restart ONLY the openlist service — never bare `compose up -d`
ssh user@cfscan "docker restart openlist"
# On restart: initStatic() reads from dist_dir, initIndex() loads the new index.html.
```

**Production path mapping** (reference):

| Host                           | Host path              | Container path         |
|--------------------------------|------------------------|------------------------|
| cfscan (op.rnarket.com)        | `/root/docker/opdata`  | `/opt/openlist/data`   |
| txcloud (trans.zoterosync.top) | `/root/docker/data`    | `/opt/openlist/data`   |

So for both hosts: `dist_dir = /opt/openlist/data/frontend_dist/dist`

After restart, `initStatic()` picks up `dist_dir` deterministically — no GitHub access required, no watcher race, no index.html staleness.

**Why not delete `frontend_dist/` entirely?** The watcher goroutine tracks state inside that directory (`.frontend_version` file, the `dist/` subdirectory). Removing the whole `frontend_dist/` directory races with the watcher. Back up `dist/` as `dist.bak.<ts>` instead; that's atomic and the watcher ignores the `.bak` sibling.

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
