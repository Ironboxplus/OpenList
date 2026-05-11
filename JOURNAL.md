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

## 2026-05-10 — 115 Rate Limiting + Concurrent Token Refresh Fix

### 问题现象

复制任务（`/scnet/` → `/storage/my_115/`）全部失败，错误 `code: 0, message:` 和 `code: 40100000, message: 参数错误！`。目录确实存在，单任务正常，多 worker 并发时全部报错。

### 根因 1：Put 方法多个 SDK 调用未走限流器

`d1b72178` fix(115_open): rate-limit every SDK call in Put method

- **发现**：`Put()` 入口只调一次 `WaitLimit`，后续 3-4 个 SDK 请求（`UploadInit` ×3 + `UploadGetToken`）直接发出，不经过限流器
- **影响**：10 个 copy worker 并发时，不受限的 Put 请求和其他走限流器的 List/MakeDir 请求一起打到 115 API，瞬时 QPS 超过 115 的限制，API 返回 `state:false, code:0`（空错误）拒绝所有请求
- **修复**：移除 Put 入口的单次 `WaitLimit`，在每个 SDK 调用（`UploadInit`、`UploadGetToken`）前单独加 `WaitLimit`
- **测试**：`TestPutRateLimitsEverySDKCall` — 设置 10 req/s 限流器，验证 3 个 UploadInit 调用之间有 >=70ms 间隔；`TestPutRateLimitsPreHashPath` — 验证秒传成功路径
- Files: `drivers/115_open/driver.go`, `drivers/115_open/driver_test.go`

### 根因 2：SDK RefreshToken 无并发保护

`823f46ba` fix(deps): bump 115-sdk-go to v0.2.6 for concurrent refresh fix

- **发现**：日志显示 `40140117 refresh frequently` 和 `40140120 refresh token error` 从 3 月 25 日就开始出现。115 的 refresh token 是一次性的——token 过期后多个 goroutine 同时调 `authRequest`，同时检测到 401，同时调 `RefreshToken`。第一个成功消耗了旧 RT，后续的全部失败（RT 已作废），token 被损坏或清空
- **时间线**（`my_115` 实例）：
  - 07:06:19 — 存储加载成功
  - 07:28:34 — copy workers 从 3 改成 10
  - 09:11:29 — 最后一条成功的 `[115] GetFiles` 日志
  - 09:11-09:45 — 35 分钟无 `[115]` 日志（全是文件上传，走 UploadInit 不产生 `[115]` 日志）
  - 09:46:01 — 首次 `40100000 参数错误`（token 已失效/清空）
  - 从 07:06 到 09:46 正好 ~2h40m，115 access token TTL 约 2h
- **修复**（SDK `v0.2.6`）：`authRequest` 中加 `refreshMu sync.Mutex` + double-check pattern。发请求前锁内读取 `usedToken`，遇 401 后锁内比对 `c.accessToken == usedToken`，若已被别的 goroutine 刷新过则跳过，未刷新才调 `RefreshToken`
- **测试**：`TestConcurrentAuthRequestRefreshesOnlyOnce` — 10 个并发 goroutine 同时打过期 token，断言 `RefreshToken` 只被调用 1 次，全部 goroutine 成功。`count=3` 稳定通过
- Files: SDK `client.go`, `request.go`, `request_test.go`; OP `go.mod`, `go.sum`

---

## 2026-05-11 — 115 GetFolderInfoByPath 空数据处理

### 问题现象

复制任务预建目标子目录时报错：`failed to get obj: json: cannot unmarshal array into Go value of type sdk.GetFolderInfoResp`。只有**不存在**的子目录报错，已存在的目录正常。

### 根因

115 Open API 的 `GetFolderInfoByPath` 在路径不存在时返回 `{state:true, data:[]}` 而不是正常的错误码。SDK 的 `authRequest` 直接把 `[]` 反序列化到 `GetFolderInfoResp`（struct）→ `json.UnmarshalTypeError`。该错误不是 `errs.ObjectNotFound`，导致 `MakeDir` 在 `op/fs.go:350` 当作未知错误抛出，而不是正常进入"目录不存在→创建"流程。

### 修复

**SDK v0.2.8**（`cf4f508` fix: return ErrDataEmpty when API responds with empty data）：
- 新增 `ErrDataEmpty` sentinel error
- `authRequest` 在 `extractData` 模式下：`data` 为 `null`/空 → 直接返回 `ErrDataEmpty`；`data` 为 `[]` 反序列化到 struct 失败 → 也返回 `ErrDataEmpty`（反序列化到 slice 类型则正常通过，不影响 `DelFile` 等返回空数组的 API）

**Driver 层**：
- `Open115.Get()` 捕获 `sdk.ErrDataEmpty` → 转为 `errs.ObjectNotFound`
- `MakeDir` 的 `errs.IsObjectNotFound` 检测通过 → 正常创建目录

### 测试

- SDK: `TestAuthRequestReturnsErrDataEmptyForEmptyArray`、`TestAuthRequestReturnsErrDataEmptyForNull`、`TestAuthRequestSucceedsForValidObject`
- Driver: `TestGetReturnsObjectNotFoundForEmptyData`、`TestGetReturnsObjForExistingFolder`
- 既有 27 个测试全部通过，含 `TestOpen115RemoveDeleteReturnsErrorWhenRecycleEntryMissing`（验证 `DelFile` 的 `data:[]` 不受影响）

### 教训：不要 force-push tag

Go module proxy 会缓存 tag 第一次发布时的内容。Force-push 更新 tag 后，`go.sum` 中记录的旧 hash 与新内容不匹配，触发 `checksum mismatch` 安全错误。正确做法是打新版号（v0.2.7 → v0.2.8）。

- Files: SDK `error.go`, `request.go`, `request_test.go`; OP `drivers/115_open/driver.go`, `drivers/115_open/driver_test.go`, `go.mod`, `go.sum`

---

## 已完成调查：FileStream.cache truncated stream（2026-04-07）

### 现象

- `failed to read all data: (expect =50331648, actual =41592644) unexpected EOF`
- 调用链：`FsStream -> fs.PutDirectly -> op.Put -> FileStream.cache`
- 115_open 与 google_drive 均出现

### 根因

上游请求体提前结束（truncated stream），`FileStream.cache()` 的 `io.ReadFull` 严格检测并报错。`50331648 = 48MiB`（MaxBufferLimit 窗口），`41592644 ≈ 39.66MiB`（实际收到的字节数）。actual 值不固定，排除驱动逻辑在固定偏移崩溃的可能。

`3b2f9d55` 将"超限时落盘"改为"裁剪到 MaxBufferLimit"，使错误暴露更早，但非根因。

### 处置

P0：在上传入口增加"声明长度 vs 实际接收长度"日志；排查反向代理超时。

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
