# Architecture — Driver System, Request Flow, Internal Packages, Startup

Deep-dive reference for the OpenList backend's core structure. See [INDEX.MD](../../INDEX.MD) for the subsystem map.

---

## Driver System (Storage Abstraction)

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
   - `json:"field_name"` — JSON field name
   - `type:"select"` — Input type (select, string, text, bool, number)
   - `required:"true"` — Required field
   - `options:"a,b,c"` — Dropdown options
   - `default:"value"` — Default value
4. Register driver in `init()` function

**Example Driver Structure**:
```go
type YourDriver struct {
    model.Storage
    Addition
    client *YourClient
}

func (d *YourDriver) Init(ctx context.Context) error { /* initialize client */ }
func (d *YourDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) { /* list files */ }
func (d *YourDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) { /* return download URL */ }
```

---

## Request Flow

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

---

## Internal Package Structure

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

---

## Startup Sequence

**Location**: `internal/bootstrap/run.go`

Order of initialization:
1. `InitConfig()` — Load config, environment variables
2. `Log()` — Initialize logging
3. `InitDB()` — Connect to database (`_busy_timeout=5000` for SQLite parallel safety)
4. `data.InitData()` — Initialize default data
5. `LoadStorages()` — Load and initialize all storage drivers (parallel by driver type)
6. `InitTaskManager()` — Start background tasks
7. `InitPlugins()` — Start wazero plugin manager
8. `Start()` — Start HTTP/HTTPS/WebDAV/FTP/SFTP servers

---

## Common Patterns

### Error Handling

Use custom errors from `internal/errs/`:
- `errs.NotImplement` — Feature not implemented
- `errs.ObjectNotFound` — File/folder not found
- `errs.NotFolder` — Path is not a directory
- `errs.StorageNotInit` — Storage driver not initialized

**Link Expiry Detection**:
```go
// Checks error string for keywords: "expired", "invalid signature", "token expired"
// Also checks HTTP status: 401, 403, 410, 500
if stream.IsLinkExpiredError(err) { /* refresh link */ }
```

### Saving Driver State

```go
d.AccessToken = newToken
op.MustSaveDriverStorage(d)  // Persists to database
```

### Rate Limiting

```go
type YourDriver struct { limiter *rate.Limiter }

func (d *YourDriver) Init(ctx context.Context) error {
    d.limiter = rate.NewLimiter(rate.Every(time.Second), 1) // 1 req/sec
}
func (d *YourDriver) List(...) {
    d.limiter.Wait(ctx)
    // Make API call
}
```

### Context Cancellation

```go
select {
case <-ctx.Done():
    return nil, ctx.Err()
default:
    // Continue operation
}
```

### Naming Conventions

- Drivers: lowercase with underscores (`baidu_netdisk`, `aliyundrive_open`)
- Packages: lowercase (`internal/op`)
- Interfaces: PascalCase with suffix (`Reader`, `Writer`)
- Driver config: use `driver.RootPath` or `driver.RootID` for root folder; add `omitempty` to optional JSON fields
- Logging: `log.Infof("[driver_name] message")` via `logrus`
- Retries: `github.com/avast/retry-go`; HTTP client timeout 30s via `base.RestyClient`
