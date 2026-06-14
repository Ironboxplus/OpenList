# OpenList Workspace — CLAUDE.md

This file covers the entire OpenList workspace, which contains three main repositories:

- **OP/** — OpenList backend (Go, main project)
- **OpenList-Frontend/** — SolidJS frontend
- **movi-player/** — FFmpeg WASM video player (local fork)

Each section below is labeled by component. The backend section is the primary development guide.

---

## Environment Configuration

| Item | Value |
|------|-------|
| OS | Windows 11 Pro for Workstations (10.0.26100) |
| Go | go1.25.4 windows/amd64 |
| Node.js | v22.19.0 |
| Project Root | `E:\Go\Openlist\` |

### Repository Map

| Directory | Repo | Branch | Remote |
|-----------|------|--------|--------|
| `OP/` | [Ironboxplus/OpenList](https://github.com/Ironboxplus/OpenList) | `feat/dynamic-frontend` | origin (fork), op (upstream: OpenListTeam/OpenList) |
| `OpenList-Frontend/` | [Ironboxplus/OpenList-Frontend](https://github.com/Ironboxplus/OpenList-Frontend) | `main` | origin (upstream: OpenListTeam/OpenList-Frontend), ironbox (fork) |
| `movi-player/` | Local fork of [MrUjjwalG/movi-player](https://github.com/MrUjjwalG/movi-player) | `main` | — |
| `115-sdk-go/` | [Ironboxplus/115-sdk-go](https://github.com/Ironboxplus/115-sdk-go) | — | — |

### Module Replacements (Backend `go.mod`)

| Module | Replace Target | Notes |
|--------|---------------|-------|
| `github.com/OpenListTeam/115-sdk-go` | `github.com/Ironboxplus/115-sdk-go v0.2.9` | Refresh context isolation, token error code filters, ErrDataEmpty, FlexString CID |
| `github.com/ProtonMail/go-proton-api` | `github.com/henrybear327/go-proton-api v1.0.0` | Community fork |
| `github.com/cronokirby/saferith` | `github.com/Da3zKi7/saferith v0.33.0-fixed` | Bug fix fork |

---

## [Backend] Core Development Principles

1. **最小代码改动原则** (Minimum code changes): Make the smallest change necessary to achieve the goal
2. **不缓存整个文件原则** (No full file caching for seekable streams): For SeekableStream, use RangeRead instead of caching entire file
3. **必要情况下可以多遍上传原则** (Multi-pass upload when necessary): If rapid upload fails, fall back to normal upload

## [Backend] Build and Development Commands

```bash
# Development
go run main.go                    # Run backend server (default port 5244)
air                              # Hot reload during development (uses .air.toml)
./build.sh dev                   # Build development version with frontend
./build.sh release               # Build release version

# Testing
go test ./...                    # Run all tests
go test ./drivers/115_open/ -v   # Run tests for a specific driver
go test ./drivers/115_open/ -run TestCheckUploadCallback -v  # Run a single test
go build ./drivers/115_open/...  # Quick compile check for a package

# Docker
docker-compose up                # Run with docker-compose
docker build -f Dockerfile .     # Build docker image
```

**Build Script Details** (`build.sh`):
- Fetches frontend from `$FRONTEND_REPO` (default: `Ironboxplus/OpenList-Frontend`) releases and embeds into `public/dist/`
- Injects version info via ldflags: `-X "github.com/OpenListTeam/OpenList/v4/internal/conf.BuiltAt=$(date +'%F %T %z')"`
- Supports `dev`, `beta`, and release builds
- Downloads prebuilt frontend distribution automatically

**Go Version**: Requires Go 1.24+ (CI uses 1.25.0)

**Module Replacements** (`go.mod`): Some dependencies use `replace` directives pointing to forks (e.g., `115-sdk-go` → `Ironboxplus/115-sdk-go`). When modifying SDK behavior, check if there's a local fork to edit.

## [Backend] Architecture Overview

### Driver System (Storage Abstraction)

OpenList uses a **driver pattern** to support 70+ cloud storage providers. Each driver implements the core `Driver` interface.

**Location**: `drivers/*/`

**Core Interfaces** (`internal/driver/driver.go`):
- `Reader`: List directories, generate download links (REQUIRED)
- `Writer`: Upload, delete, move files (optional)
- `ArchiveDriver`: Extract archives (optional)
- `LinkCacheModeResolver`: Custom cache TTL strategies (optional)

**Driver Registration Pattern**:
```go
// In drivers/your_driver/meta.go
var config = driver.Config{
    Name:        "YourDriver",
    LocalSort:   false,
    NoCache:     false,
    DefaultRoot: "/",
}

func init() {
    op.RegisterDriver(func() driver.Driver {
        return &YourDriver{}
    })
}
```

**Adding a New Driver**:
1. Copy `drivers/template/` to `drivers/your_driver/`
2. Implement `List()` and `Link()` methods (required)
3. Define `Addition` struct with configuration fields using struct tags:
   - `json:"field_name"` - JSON field name
   - `type:"select"` - Input type (select, string, text, bool, number)
   - `required:"true"` - Required field
   - `options:"a,b,c"` - Dropdown options
   - `default:"value"` - Default value
4. Register driver in `init()` function

**Example Driver Structure**:
```go
type YourDriver struct {
    model.Storage
    Addition
    client *YourClient
}

func (d *YourDriver) Init(ctx context.Context) error {
    // Initialize client, login, etc.
}

func (d *YourDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
    // Return list of files/folders
}

func (d *YourDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
    // Return download URL or RangeReader
}
```

### Request Flow

```
HTTP Request (Gin Router)
    ↓
Middleware (Auth, CORS, Logging)
    ↓
Handler (server/handles/)
    ↓
fs.List/Get/Link (mount path → storage path conversion)
    ↓
op.List/Get/Link (caching, driver lookup)
    ↓
Driver.List/Link (storage-specific API calls)
    ↓
Response (JSON / Proxy / Redirect)
```

### Internal Package Structure

| Package | Purpose |
|---------|---------|
| `bootstrap/` | Initialization sequence: config, DB, storages, servers |
| `conf/` | Configuration management |
| `db/` | Database models (SQLite/MySQL/Postgres) |
| `driver/` | Driver interface definitions |
| `fs/` | Mount path abstraction (converts `/mount/path` to storage + path) |
| `op/` | Core operations with caching and driver management |
| `stream/` | Streaming, range readers, link refresh, rate limiting |
| `model/` | Data models (Obj, Link, Storage, User) |
| `cache/` | Multi-level caching (directories, links, users, settings) |
| `net/` | HTTP utilities, proxy config, download manager |

### Link Generation and Caching

**Link Types**:
1. **Direct URL** (`link.URL`): Simple redirect to storage provider
2. **RangeReader** (`link.RangeReader`): Custom streaming implementation
3. **Refreshable Link** (`link.Refresher`): Auto-refresh on expiration

**Cache System** (`internal/op/cache.go`):
- **Directory Cache**: Stores file listings with configurable TTL
- **Link Cache**: Stores download URLs (30min default)
- **User Cache**: Authentication data (1hr default)
- **Custom Policies**: Pattern-based TTL via `pattern:ttl` format

**Cache Key Pattern**: `{storageMountPath}/{relativePath}`

**Invalidation**: Recursive tree deletion for directory operations

### Range Reader and Streaming

**Location**: `internal/stream/`

**Purpose**: Handle partial content requests (HTTP 206), multi-threaded downloads, and link refresh during streaming.

**Key Components**:

1. **RangeReaderIF**: Core interface for range-based reading
   ```go
   type RangeReaderIF interface {
       RangeRead(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error)
   }
   ```

2. **RefreshableRangeReader**: Wraps RangeReader with automatic link refresh
   - Detects expired links via error strings or HTTP status codes (401, 403, 410, 500)
   - Calls `link.Refresher(ctx)` to get new link
   - Resumes download from current byte position
   - Max 3 refresh attempts to prevent infinite loops

3. **Multi-threaded Downloader** (`internal/net/downloader.go`):
   - Splits file into parts based on `Concurrency` and `PartSize`
   - Downloads parts in parallel
   - Assembles final stream

**Stream Types and Reader Management**:

⚠️ **CRITICAL**: SeekableStream.Reader must NEVER be created early!

- **FileStream**: One-time sequential stream (e.g., HTTP body)
  - `Reader` is set at creation and consumed sequentially
  - Cannot be rewound or re-read

- **SeekableStream**: Reusable stream with RangeRead capability
  - Has `rangeReader` for creating new readers on-demand
  - `Reader` should ONLY be created when actually needed for sequential reading
  - **DO NOT create Reader early** - use lazy initialization via `generateReader()`

**Common Pitfall - Early Reader Creation**:
```go
// ❌ WRONG: Creating Reader early
if _, ok := rr.(*model.FileRangeReader); ok {
    rc, _ := rr.RangeRead(ctx, http_range.Range{Length: -1})
    fs.Reader = rc  // This will be consumed by intermediate operations!
}

// ✅ CORRECT: Let generateReader() create it on-demand
// Reader will be created only when Read() is called
return &SeekableStream{FileStream: fs, rangeReader: rr}, nil
```

**Why This Matters**:
- Hash calculation uses `StreamHashFile()` which reads the file via RangeRead
- If Reader is created early, it may be at EOF when HTTP upload actually needs it
- Result: `http: ContentLength=X with Body length 0` error

**Hash Calculation for Uploads**:
```go
// For SeekableStream: Use RangeRead to avoid consuming Reader
if _, ok := file.(*SeekableStream); ok {
    hash, err = stream.StreamHashFile(file, utils.MD5, 40, &up)
    // StreamHashFile uses RangeRead internally, Reader remains unused
}

// For FileStream: Must cache first, then calculate hash
_, hash, err = stream.CacheFullAndHash(file, &up, utils.MD5)
```

**Link Refresh Pattern**:
```go
// In op.Link(), a refresher is automatically attached
link.Refresher = func(refreshCtx context.Context) (*model.Link, model.Obj, error) {
    // Get fresh link from storage driver
    file, err := GetUnwrap(refreshCtx, storage, path)
    newLink, err := storage.Link(refreshCtx, file, args)
    return newLink, file, nil
}

// RefreshableRangeReader uses this during streaming
if IsLinkExpiredError(err) && r.link.Refresher != nil {
    newLink, _, err := r.link.Refresher(ctx)
    // Resume from current position
}
```

**Proxy Function** (`server/common/proxy.go`):

Handles multiple scenarios:
1. Multi-threaded download (`link.Concurrency > 0`)
2. Direct RangeReader (`link.RangeReader != nil`)
3. Refreshable link (`link.Refresher != nil`) ← Wraps with RefreshableRangeReader
4. Transparent proxy (forwards to `link.URL`)

### Frontend Dist Serving

**Location**: `server/static/static.go`, `internal/frontend/`

The frontend dist has two sources, with a strict priority:

1. **Embedded dist** (`public/dist/` via `go:embed`): Baked into the binary at build time. Always used on startup.
2. **Dynamic dist** (fetched by watcher): The `frontend.Watcher` checks GitHub every 30 minutes for a newer rolling release. If found, it downloads to `data/frontend_dist/` and hot-swaps the serving FS via `ReloadStatic()`.

**Key design rule**: `initStatic()` always starts with the embedded dist (or `dist_dir` if configured). The cached dynamic dist in the data volume is never read on startup — only the watcher can activate it after verifying a newer version exists on GitHub. This prevents stale cache from overriding a newer Docker image.

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

### Startup Sequence

**Location**: `internal/bootstrap/run.go`

Order of initialization:
1. `InitConfig()` - Load config, environment variables
2. `Log()` - Initialize logging
3. `InitDB()` - Connect to database
4. `data.InitData()` - Initialize default data
5. `LoadStorages()` - Load and initialize all storage drivers
6. `InitTaskManager()` - Start background tasks
7. `Start()` - Start HTTP/HTTPS/WebDAV/FTP/SFTP servers

## [Backend] Common Patterns

### Error Handling

Use custom errors from `internal/errs/`:
- `errs.NotImplement` - Feature not implemented
- `errs.ObjectNotFound` - File/folder not found
- `errs.NotFolder` - Path is not a directory
- `errs.StorageNotInit` - Storage driver not initialized

**Link Expiry Detection**:
```go
// Checks error string for keywords: "expired", "invalid signature", "token expired"
// Also checks HTTP status: 401, 403, 410, 500
if stream.IsLinkExpiredError(err) {
    // Refresh link
}
```

### Upload and OSS Callback Validation

Drivers that upload via Aliyun OSS (e.g., `115`, `115_open`) use a callback mechanism: after OSS stores the file, it POSTs to the storage provider's callback URL. The provider returns a JSON response indicating whether the file was registered.

**Critical**: Always capture and validate the callback response:
```go
var bodyBytes []byte
_, err = bucket.CompleteMultipartUpload(imur, parts,
    oss.Callback(base64.StdEncoding.EncodeToString([]byte(callback))),
    oss.CallbackVar(base64.StdEncoding.EncodeToString([]byte(callbackVar))),
    oss.CallbackResult(&bodyBytes),  // ← MUST capture this
)
// Check both OSS error AND callback response
if err != nil { return err }
// Parse bodyBytes to verify {"state": true}
```

Without `oss.CallbackResult`, the upload appears successful (OSS returns 200) but the file is never registered on the provider's side. The local cache shows the file temporarily, but it vanishes on refresh.

**`Put` vs `PutResult`**: Drivers can implement either `driver.Put` (returns `error`) or `driver.PutResult` (returns `model.Obj, error`). When `Put` returns nil, `op.Put` creates a temporary object in the directory cache. When `PutResult` returns an actual object, that object is used in the cache instead.

### HybridCache replaces the old `MaxBufferLimit` truncation

Historically `CacheFullAndHash` → `CacheFullAndWriter` → `cache()` capped buffering at `MaxBufferLimit` (~48MB), so non-seekable streams larger than that produced a truncated SHA1 (only the first 48MB hashed) and providers like 115 rejected the upload. The 115_open driver carried a `utils.CreateTempFile` workaround.

**Current (2026-05-17)**: upstream PR #2460 unified caching via `internal/mem.HybridCache` — three tiers (heap memory → `LinearMemory` → temp file fallback) with 16MB blocks (`MaxBlockLimit`). `CacheFullAndWriter` now writes the *entire* stream regardless of size, so the truncation bug is gone for **all drivers**, not just 115_open. The 115_open workaround was removed; the standard `stream.CacheFullAndHash` path is back.

**Unaffected paths**:
- `SeekableStream` (copy tasks): uses `RangeRead` directly, never goes through `cache()`
- Form uploads: `c.FormFile()` already stores to a temp file, so `GetFile()` returns non-nil and `CacheFullAndWriter` reads the entire file

### Pass 2 prefetch on `hybridSectionReader`

For sequential multipart uploads (115_open, baidu_netdisk, aliyundrive_open, etc.), `hybridSectionReader.GetSectionReader` launches a background goroutine to pre-read the next chunk while the caller uploads the current one. The next call picks up the prefetched block; mismatched offsets fall back to synchronous read. Prefetch is clamped to remaining file size, drains on `DiscardSection`, and surfaces errors on the next `GetSectionReader`. Cleanup is registered via `file.Add` so the goroutine is drained before `HybridCache` is freed.

Drivers that already use `errgroup.Lifecycle` (e.g. 123/upload.go) with `Before` (List/GetSectionReader) and `Do` (upload) get overlap from the lifecycle pattern itself; the section-reader prefetch helps drivers that loop sequentially without Before/Do split (e.g. 115_open, which requires `oss.Sequential()`).

### Merge-task ObjectNotFound tolerance

Merge-mode `FileTransferTask.RunWithNextTaskCallback` calls `op.List(dst)` to build the `existedObjs` skip set. A non-existent dst must be treated as "empty" rather than fatal (otherwise resuming an interrupted merge to a fresh dst fails immediately). The logic is extracted into `existingDstFilesFn` in `internal/fs/copy_move.go`:

```go
dstObjs, err := listDst(ctx, dstPath)
if err != nil && !errors.Is(err, errs.ObjectNotFound) {
    return nil, errors.WithMessagef(err, "failed list dst [%s] objs", dstPath)
}
// non-existent dst → empty map, merge proceeds and creates dst on demand
```

A previous BFS-style "precreate one level of subdirectories" optimization was removed (2026-05-17, commit `6b3ce577`); directory creation now happens on demand via `op.Put`'s internal `MakeDir(parent)` and `op.MakeDir`'s recursive parent walk. The top-level dst `MakeDir` at `copy_move.go:198` is kept for early failure detection. 12 unit tests on `existingDstFilesFn` (including raw + wrapped `ObjectNotFound`) lock in the contract.

### Saving Driver State

When updating tokens or credentials:
```go
d.AccessToken = newToken
op.MustSaveDriverStorage(d)  // Persists to database
```

### Rate Limiting

Use `rate.Limiter` for API rate limits:
```go
type YourDriver struct {
    limiter *rate.Limiter
}

func (d *YourDriver) Init(ctx context.Context) error {
    d.limiter = rate.NewLimiter(rate.Every(time.Second), 1) // 1 req/sec
}

func (d *YourDriver) List(...) {
    d.limiter.Wait(ctx)
    // Make API call
}
```

### Context Cancellation

Always respect context cancellation in long operations:
```go
select {
case <-ctx.Done():
    return nil, ctx.Err()
default:
    // Continue operation
}
```

## [Backend] Important Conventions

**Naming**:
- Drivers: lowercase with underscores (e.g., `baidu_netdisk`, `aliyundrive_open`)
- Packages: lowercase (e.g., `internal/op`)
- Interfaces: PascalCase with suffix (e.g., `Reader`, `Writer`)

**Driver Configuration Fields**:
- Use `driver.RootPath` or `driver.RootID` for root folder
- Add `omitempty` to optional JSON fields
- Use descriptive help text in struct tags

**Retries and Timeouts**:
- Use `github.com/avast/retry-go` for retry logic
- Set reasonable timeouts on HTTP clients (default 30s in `base.RestyClient`)
- For unstable APIs, implement exponential backoff

**Logging**:
- Use `logrus` via `log` package
- Levels: `log.Debugf`, `log.Infof`, `log.Warnf`, `log.Errorf`
- Include driver name in logs: `log.Infof("[driver_name] message")`

## [Backend] Project Context

OpenList is a community-driven fork of AList, focused on:
- Long-term governance and trust
- Support for 70+ cloud storage providers
- Web UI for file management
- Multi-protocol support (HTTP, WebDAV, FTP, SFTP, S3)
- Offline downloads (Aria2, Transmission)
- Full-text search
- Archive extraction

**License**: AGPL-3.0

---

## Frontend (OpenList-Frontend)

SolidJS + Vite + Hope UI. Builds to `dist/` which gets embedded into the backend binary via `go:embed`.

### Build Commands

```bash
pnpm install                    # Install dependencies
pnpm dev                        # Dev server (port 5173)
pnpm build                      # Production build
pnpm test                       # Run vitest tests
```

### Preview System

Previews registered in `src/pages/home/previews/index.ts`. Each preview declares `prior: true/false` for priority ordering. Current video priority: movi-player (default) > Artplayer (fallback).

### Movi Player Integration

movi-player (FFmpeg WASM + WebCodecs) is the default video player, with full subtitle support:
- **SRT/VTT**: movi-player native parsing
- **ASS (external)**: JASSUB (libass WASM) overlay canvas rendering with font fallback
- **PGS/SUP (external)**: libpgs overlay canvas rendering
- **Embedded subtitles**: movi-player WASM demuxer handles all embedded formats

Requires COOP/COEP headers (`Cross-Origin-Opener-Policy: same-origin`, `Cross-Origin-Embedder-Policy: credentialless`) for SharedArrayBuffer. Set in backend `server/router.go`.

---

## Movi Player Fork (`movi-player/`)

Clone of [MrUjjwalG/movi-player](https://github.com/MrUjjwalG/movi-player). FFmpeg WASM + WebCodecs browser-side video decoder.

### Fork Changes

- `src/core/MoviPlayer.ts`: Added `parseASS()` + `parseASSTime()` for external ASS subtitle support
- Format detection in `selectSubtitleLang()` extended for `.ass`/`.ssa`

### Build

```bash
npm run build:wasm    # Requires Docker (C → WASM)
npm run build:ts      # TypeScript only (needs dist/wasm/movi.js)
npm run build         # Full build (wasm + ts)
```

### Known Limitations

| Limitation | Reason |
|-----------|--------|
| External ASS: text only (no styles) | SubtitleCue architecture is plain text. Full ASS → use JASSUB overlay |
| External PGS: not supported in movi-player native | Binary format needs demuxer. Handled by libpgs overlay |
| Dolby Vision: purple tint | WASM decoder lacks DV enhancement layer |
| Requires COOP/COEP headers | SharedArrayBuffer for WASM threads |

---

## 115-sdk-go Fork

Fork of upstream SDK at `github.com/Ironboxplus/115-sdk-go`. Key changes (v0.2.9):
- **Concurrent token refresh**: `authRequest` uses `sync.Mutex` + double-check to prevent multiple goroutines from racing on `RefreshToken`
- **Refresh context isolation** (v0.2.9): `RefreshToken` runs under `context.WithTimeout(context.WithoutCancel(ctx), 30s)` — once a refresh starts, caller cancellation cannot abort it. Fixes token loss when video player cancels mid-refresh
- **Token error code filters** (v0.2.9): `shouldRefreshToken(code)` excludes 40140117 (CodeRefreshFrequently) and 40140120 (CodeRefreshTokenError) from triggering refresh; prevents feeding rate limiter on unrecoverable auth failures
- **ErrDataEmpty sentinel**: `GetFolderInfoByPath` returning `data:[]` for non-existent paths → returns `ErrDataEmpty` instead of unmarshal error
- **FlexString CID**: handles numeric/string JSON interop for category IDs
