# Development Journal

Chronological log of all changes in this fork, from earliest to latest.

---

## 2026-04-25 — Initial Feature Batch (rebased onto up/main)

### `17be63fb` feat(stream): link refresh, self-healing reader, seekable prefetch, and upload hash rework
- Added `RefreshableRangeReader`: wraps `RangeReader` with automatic link refresh on expiry (max 50 attempts)
- Added `selfHealingReadCloser`: detects 0-byte reads, connection resets, and `io.ErrUnexpectedEOF`; reconnects from current offset transparently
- Added `IsLinkExpiredError()`: checks error strings + HTTP 4xx status codes for link expiry
- Added 2-window async prefetch in `directSectionReader`: while uploading chunk N, prefetch chunk N+1 via goroutine
- Reworked `StreamHashFile` to use `RangeRead` for `SeekableStream` (never consumes `Reader`)
- Added `ReadFullWithRangeRead` with retry (max 5 attempts, 1-5s backoff)
- Files: `internal/stream/util.go`, `internal/stream/stream.go`

### `1db9136f` feat(google_drive): duplicate filename handling, folder lock, retry, and MD5 checksum
- Added `mkdirLocks` (`sync.Map`) to prevent concurrent creation of duplicate folders
- Added existence check with retry before folder creation
- Added 500ms consistency delay after folder creation
- Added MD5 hash computation for upload integrity (via `StreamHashFile`)
- Added `chunkUpload` with retry and `RangeRead`-based streaming
- Files: `drivers/google_drive/driver.go`, `drivers/google_drive/util.go`

### `f9bc1567` feat(115_open): permanent delete, proxy_range, offline task fixes, and error handling
- Added `RemoveWay` config option: "trash" (default) or "delete" (permanent)
- Implemented `removePermanently`: deletes from recycle bin after trash
- Added `findRecycleBinEntry` with paginated recycle bin search
- Added `matchRecycleBinEntry` with multi-strategy matching (ID → SHA1 → name+size)
- Added `findRecycleBinEntryWithRetry` (4 attempts, 300ms backoff) for eventual consistency
- Added `FlexString` CID handling (numeric/string JSON interop)
- Added `proxy_range` option exposure
- Files: `drivers/115_open/driver.go`, `drivers/115_open/meta.go`, `drivers/115_open/upload.go`, `drivers/115_open/driver_test.go`

### `f1493fc9` feat(offline_download): multi-page task retrieval and task limit wait mechanism
- 115 `OfflineList` now paginates through all pages (was only page 1)
- Added task limit wait mechanism in offline download client
- Moved 115 offline task cleanup from `Run()` to `Update()` so it runs even if transfer fails
- Files: `internal/offline_download/115_open/client.go`, `internal/offline_download/tool/download.go`

### `35130094` feat(drivers): baidu streaming upload, quark rate-limit/retry, 123pan etag fix
- **Baidu Netdisk**: extracted upload logic to `upload.go`, full streaming upload support
- **Quark Open**: added rate limiter, retry with chunk size adjustment on 413 error
- **123 Open**: fixed copy failure due to incorrect etag, added SHA1 rapid upload
- Normalized `StreamHashFile` progress weight to 100 across all drivers
- Files: `drivers/baidu_netdisk/upload.go`, `drivers/quark_open/driver.go`, `drivers/123_open/driver.go`

### `7787ddbc` fix(core): copy_move depth, alias storage retrieval, sftp symlink, 500 panic
- Fixed `preCreateDirectoryTree` depth from 2 to 1 to avoid deep recursion
- Fixed alias storage retrieval method in `listRoot`
- Fixed SFTP symlink handling
- Fixed 500 panic and NaN issues
- Files: `internal/fs/copy_move.go`, `drivers/alias/util.go`, `drivers/sftp/types.go`

### `4e735df0` feat(frontend): dynamic frontend fetching, CI upgrades, and build infrastructure
- Added `internal/frontend/fetcher.go`: auto-download frontend dist from GitHub releases
- Added `internal/frontend/watcher.go`: periodic check (30min) for new versions
- Added `FrontendRepoDefault` ldflags variable for build-time frontend repo injection
- CI: action version upgrades, frontend caching, version matrix builds
- `build.sh`: frontend repo configurable via `FRONTEND_REPO` env var
- Files: `internal/frontend/`, `.github/workflows/`, `build.sh`, `internal/conf/`, `server/static/`

### `0bf12c5c` chore: add project docs and update dependencies (115-sdk-go fork)
- Added `CLAUDE.md` with comprehensive project guidance
- Added `COMPATIBILITY_REPORT.md` for 115-sdk-go fork analysis
- Updated 115-sdk-go dependency to `v0.2.5` (FlexString CID support)
- Files: `CLAUDE.md`, `COMPATIBILITY_REPORT.md`, `go.mod`, `go.sum`

---

## 2026-05-08 — Rebase onto upstream + Code Review Fixes

### Rebase onto `up/main` @ `c7c0cfae`
- `feat/dynamic-frontend` cleanly rebased (8 commits, 0 conflicts)
- Upstream additions included: path validation, SplitSeq perf, ObjectAlreadyExists check, qBittorrent login fix, custom share IDs, Getter interfaces (webdav/s3/115_open), about page logo fix

### `0369bc0a` fix: bounded auth retry, mkdirLocks cleanup, EOF handling, tar size limit, dist swap lock, Go 1.26 vet
Code review identified 16 issues (2 CRITICAL, 5 HIGH). Fixed 6:
- **Google Drive Put 401 recursion** (CRITICAL): replaced infinite recursive `d.Put()` call with `putWithRetry()` + `maxPutAuthRetries=2`
- **mkdirLocks memory leak** (HIGH): added `defer mkdirLocks.Delete(lockKey)` after unlock
- **selfHealingReadCloser EOF** (HIGH): removed `io.EOF` from reconnect trigger, kept only `io.ErrUnexpectedEOF`
- **Frontend tar extraction** (HIGH): added `maxExtractFileSize=500MB` + `io.LimitReader` + `hdr.Size` check
- **Frontend dist swap TOCTOU** (HIGH): added `distSwapMu` mutex around rename window
- **RefreshableRangeReader concurrency** (CRITICAL): documented that local `reader` copy is safe after `innerReader` replacement
- **Go 1.26 vet**: fixed `fmt.Errorf` non-constant format strings across 8 drivers/packages
- Added tests: `drivers/google_drive/driver_test.go`, stream EOF tests, frontend oversized file test
- Files: 14 files changed, +222/-22

---

## 2026-05-09 — OSS Upload Fix + Hash Prefetch

### `214881b3` fix(115_open): handle OSS upload timeout and PartAlreadyExist retry
- **Root cause**: `ResponseHeaderTimeout=15s` in shared `NewHttpClient()` was too short for uploading 20MB OSS parts. Timeout caused part to be uploaded but unconfirmed; retry hit `PartAlreadyExist` (409).
- **Fix 1**: Created `NewOSSUploadHttpClient()` with `ResponseHeaderTimeout=5min` dedicated to OSS uploads
- **Fix 2**: In `multpartUpload` retry, detect `PartAlreadyExist` → call `ListUploadedParts` to recover the part's ETag → treat as success
- Added `isPartAlreadyExistError()` helper
- Tests: `upload_test.go` (PartAlreadyExist detection), `oss_test.go` (upload client timeout)
- Files: `drivers/115_open/upload.go`, `internal/net/oss.go`

### `94591821` perf(stream): add double-buffer prefetch to StreamHashFile for SeekableStream
- **Before**: hash calculation read 10MB chunks sequentially (network idle during hash computation)
- **After**: while hashing chunk N, goroutine prefetches chunk N+1 via `ReadFullWithRangeRead`
- Extracted `streamHashSeekableWithPrefetch()` with double-buffering pattern
- Hash values identical — no change to upload flow or rapid-upload logic
- `FileStream` path unchanged (sequential read)
- Test: `TestStreamHashFile_SeekablePrefetchProducesSameHash`
- Files: `internal/stream/util.go`, `internal/stream/util_test.go`

---

## Architecture Notes

### Upload Data Flow (Current)
```
Pass 1: Hash Calculation (with prefetch)
  StreamHashFile → ReadFullWithRangeRead
    chunk N: hash computation (CPU)
    chunk N+1: async prefetch (network I/O)  ← overlapped

Pass 2: Multipart Upload (with prefetch)
  directSectionReader.GetSectionReader
    chunk N: upload to cloud (network I/O)
    chunk N+1: async prefetch (network I/O)  ← overlapped
```

### Proxy Architecture
- `conf.Conf.ProxyAddress` → global HTTP proxy for all server-side traffic
- Per-storage `WebProxy` / `DownProxyURL` → browser download only, NOT copy tasks
- OSS uploads use dedicated `NewOSSUploadHttpClient()` with longer timeouts
