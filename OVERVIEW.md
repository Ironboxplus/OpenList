# OpenList Fork — Project Overview

This is a fork of [OpenListTeam/OpenList](https://github.com/OpenListTeam/OpenList) (upstream), maintained at [Ironboxplus/OpenList](https://github.com/Ironboxplus/OpenList).

## Branch Structure

| Branch | Purpose |
|--------|---------|
| `feat/dynamic-frontend` | **Active development branch** (default). Rebased on `up/main`. |
| `copy` | Legacy branch with unsquashed commits. Superseded by `feat/dynamic-frontend`. |
| `main` | Synced from upstream via rebase workflow. |

## Documentation Index

| File | Description |
|------|-------------|
| [CLAUDE.md](CLAUDE.md) | AI coding guidance: architecture, driver system, stream/upload internals, conventions |
| [COMPATIBILITY_REPORT.md](COMPATIBILITY_REPORT.md) | 115-sdk-go fork compatibility analysis |
| [OVERVIEW.md](OVERVIEW.md) | This file — project index and high-level map |
| [JOURNAL.md](JOURNAL.md) | Chronological development log with all changes |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Upstream contribution guidelines |
| [README.md](README.md) | Upstream project README |
| [SECURITY.md](SECURITY.md) | Security policy |

## Key Subsystems Modified (vs Upstream)

### 1. Full-Streaming Upload with Async Prefetch
- **Files**: `internal/stream/util.go`, `internal/stream/stream.go`
- Two-pass flow: hash calculation (with double-buffer prefetch) → multipart upload (with 2-window async prefetch)
- `SeekableStream` uses `RangeRead` exclusively, never consumes the `Reader`
- `selfHealingReadCloser`: transparent link refresh + reconnect-from-offset on stream interruption
- `RefreshableRangeReader`: auto-refresh expired download links (up to 50 retries)

### 2. Dynamic Frontend Fetcher
- **Files**: `internal/frontend/fetcher.go`, `internal/frontend/watcher.go`
- Auto-downloads frontend dist from GitHub releases on startup (when `WebVersion` is rolling/beta/dev)
- `Watcher` polls every 30 min for new versions, hot-swaps dist directory
- Configurable `FrontendRepo` (default: `OpenListTeam/OpenList-Frontend`, overridable per deployment)
- `FrontendRepoDefault` injectable via ldflags at build time

### 3. Driver Enhancements

| Driver | Changes |
|--------|---------|
| 115 Open | Permanent delete with recycle-bin retry, FlexString CID, OSS upload timeout fix, PartAlreadyExist recovery, proxy_range, offline task multi-page |
| Google Drive | Duplicate filename handling, per-folder MakeDir lock, bounded 401 retry, MD5 checksum, mkdirLocks cleanup |
| Baidu Netdisk | Full streaming upload (extracted from monolithic driver) |
| Quark Open | Rate limiting, retry with chunk size adjustment |
| 123 Open | SHA1 rapid upload, etag fix, StreamHashFile progress normalization |
| Aliyundrive Open | StreamHashFile progress normalization |

### 4. CI/CD & Build
- **Files**: `.github/workflows/`, `build.sh`
- Frontend version matrix (rolling + latest) for Docker builds
- `FrontendRepoDefault` x-flag in CI
- Action version upgrades (checkout v6, setup-go v6, cache v5)
- Frontend caching in CI to avoid redundant downloads
- Static linking verification for musl builds

### 5. Offline Download
- **Files**: `internal/offline_download/115_open/`, `internal/offline_download/tool/`
- Multi-page task retrieval for 115
- Task limit wait mechanism
- Cleanup moved to `Update()` to ensure it runs even if transfer fails

## Upstream Sync

Remote `up` points to `https://github.com/OpenListTeam/OpenList.git`.

```bash
git fetch up
git rebase up/main   # on feat/dynamic-frontend
```

Current base: `up/main` @ `c7c0cfae` (2026-05-06)

## Global Proxy

All server-side HTTP traffic (driver API calls, copy/move transfers, frontend fetching) uses `conf.Conf.ProxyAddress` (global setting). Per-storage `WebProxy`/`DownProxyURL` only affects browser-facing download behavior, NOT server-side copy tasks.
