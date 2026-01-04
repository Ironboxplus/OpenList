# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build and Development Commands

```bash
# Development
go run main.go                    # Run backend server (default port 5244)
air                              # Hot reload during development (uses .air.toml)
./build.sh dev                   # Build development version with frontend
./build.sh release               # Build release version

# Testing
go test ./...                    # Run all tests

# Docker
docker-compose up                # Run with docker-compose
docker build -f Dockerfile .     # Build docker image
```

**Build Script Details** (`build.sh`):
- Fetches frontend from OpenListTeam/OpenList-Frontend releases
- Injects version info via ldflags: `-X "github.com/OpenListTeam/OpenList/v4/internal/conf.BuiltAt=$(date +'%F %T %z')"`
- Supports `dev`, `beta`, and release builds
- Downloads prebuilt frontend distribution automatically

**Go Version**: Requires Go 1.23.4+

## Architecture Overview

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

## Common Patterns

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

## Important Conventions

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

## Project Context

OpenList is a community-driven fork of AList, focused on:
- Long-term governance and trust
- Support for 70+ cloud storage providers
- Web UI for file management
- Multi-protocol support (HTTP, WebDAV, FTP, SFTP, S3)
- Offline downloads (Aria2, Transmission)
- Full-text search
- Archive extraction

**License**: AGPL-3.0
