# Streaming and Caching — Link Types, Cache System, RangeReader, SeekableStream, HybridCache

Deep-dive reference for OpenList's streaming pipeline and caching layer. See [INDEX.MD](../../INDEX.MD) for the subsystem map.

---

## Link Generation and Caching

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

---

## Range Reader and Streaming

**Location**: `internal/stream/`

**Purpose**: Handle partial content requests (HTTP 206), multi-threaded downloads, and link refresh during streaming.

**Key Components**:

1. **RangeReaderIF** — Core interface for range-based reading:
   ```go
   type RangeReaderIF interface {
       RangeRead(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error)
   }
   ```

2. **RefreshableRangeReader** — Wraps RangeReader with automatic link refresh:
   - Detects expired links via error strings or HTTP status codes (401, 403, 410, 500)
   - Calls `link.Refresher(ctx)` to get new link
   - Resumes download from current byte position
   - Max 3 refresh attempts to prevent infinite loops

3. **Multi-threaded Downloader** (`internal/net/downloader.go`):
   - Splits file into parts based on `Concurrency` and `PartSize`
   - Downloads parts in parallel; assembles final stream

---

## Stream Types and Reader Management

> **CRITICAL**: `SeekableStream.Reader` must NEVER be created early!

- **FileStream**: One-time sequential stream (e.g., HTTP body)
  - `Reader` is set at creation and consumed sequentially
  - Cannot be rewound or re-read

- **SeekableStream**: Reusable stream with RangeRead capability
  - Has `rangeReader` for creating new readers on-demand
  - `Reader` should ONLY be created when actually needed for sequential reading
  - **DO NOT create Reader early** — use lazy initialization via `generateReader()`

**Common Pitfall — Early Reader Creation**:
```go
// WRONG: Creating Reader early
if _, ok := rr.(*model.FileRangeReader); ok {
    rc, _ := rr.RangeRead(ctx, http_range.Range{Length: -1})
    fs.Reader = rc  // This will be consumed by intermediate operations!
}

// CORRECT: Let generateReader() create it on-demand
// Reader will be created only when Read() is called
return &SeekableStream{FileStream: fs, rangeReader: rr}, nil
```

**Why This Matters**:
- Hash calculation uses `StreamHashFile()` which reads the file via RangeRead
- If Reader is created early, it may be at EOF when HTTP upload actually needs it
- Result: `http: ContentLength=X with Body length 0` error

---

## Hash Calculation for Uploads

```go
// For SeekableStream: Use RangeRead to avoid consuming Reader
if _, ok := file.(*SeekableStream); ok {
    hash, err = stream.StreamHashFile(file, utils.MD5, 40, &up)
    // StreamHashFile uses RangeRead internally, Reader remains unused
}

// For FileStream: Must cache first, then calculate hash
_, hash, err = stream.CacheFullAndHash(file, &up, utils.MD5)
```

---

## Link Refresh Pattern

```go
// In op.Link(), a refresher is automatically attached
link.Refresher = func(refreshCtx context.Context) (*model.Link, model.Obj, error) {
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

---

## Proxy Function (`server/common/proxy.go`)

Handles multiple scenarios in priority order:
1. Multi-threaded download (`link.Concurrency > 0`)
2. Direct RangeReader (`link.RangeReader != nil`)
3. Refreshable link (`link.Refresher != nil`) — wraps with RefreshableRangeReader
4. Transparent proxy (forwards to `link.URL`)

---

## HybridCache — Replacing the Old MaxBufferLimit Truncation

**Background**: Historically `CacheFullAndHash` → `CacheFullAndWriter` → `cache()` capped buffering at `MaxBufferLimit` (~48MB), so non-seekable streams larger than that produced a truncated SHA1 (only the first 48MB hashed) and providers like 115 rejected the upload. The 115_open driver carried a `utils.CreateTempFile` workaround.

**Current (2026-05-17)**: upstream PR #2460 unified caching via `internal/mem.HybridCache` — three tiers (heap memory → `LinearMemory` → temp file fallback) with 16MB blocks (`MaxBlockLimit`). `CacheFullAndWriter` now writes the *entire* stream regardless of size, so the truncation bug is gone for **all drivers**, not just 115_open. The 115_open workaround was removed; the standard `stream.CacheFullAndHash` path is back.

**Unaffected paths**:
- `SeekableStream` (copy tasks): uses `RangeRead` directly, never goes through `cache()`
- Form uploads: `c.FormFile()` already stores to a temp file, so `GetFile()` returns non-nil and `CacheFullAndWriter` reads the entire file

---

## Pass 2 Prefetch on `hybridSectionReader`

For sequential multipart uploads (115_open, baidu_netdisk, aliyundrive_open, etc.), `hybridSectionReader.GetSectionReader` launches a background goroutine to pre-read the next chunk while the caller uploads the current one. The next call picks up the prefetched block; mismatched offsets fall back to synchronous read. Prefetch is clamped to remaining file size, drains on `DiscardSection`, and surfaces errors on the next `GetSectionReader`. Cleanup is registered via `file.Add` so the goroutine is drained before `HybridCache` is freed.

Drivers that already use `errgroup.Lifecycle` (e.g. 123/upload.go) with `Before` (List/GetSectionReader) and `Do` (upload) get overlap from the lifecycle pattern itself; the section-reader prefetch helps drivers that loop sequentially without Before/Do split (e.g. 115_open, which requires `oss.Sequential()`).
