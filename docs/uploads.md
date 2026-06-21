# Uploads — OSS Callback Validation, Put vs PutResult, Merge-Task Tolerance

Deep-dive reference for OpenList's upload pipeline. See [INDEX.MD](../../INDEX.MD) for the subsystem map.

---

## OSS Callback Validation

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

---

## `Put` vs `PutResult`

Drivers can implement either:
- `driver.Put` (returns `error`): When `Put` returns nil, `op.Put` creates a temporary object in the directory cache.
- `driver.PutResult` (returns `model.Obj, error`): When `PutResult` returns an actual object, that object is used in the cache instead.

Use `PutResult` when the storage provider returns metadata about the uploaded file (ID, size, timestamps), so the cache entry is accurate without requiring a re-list.

---

## Merge-Task ObjectNotFound Tolerance

Merge-mode `FileTransferTask.RunWithNextTaskCallback` calls `op.List(dst)` to build the `existedObjs` skip set. A non-existent dst must be treated as "empty" rather than fatal (otherwise resuming an interrupted merge to a fresh dst fails immediately). The logic is extracted into `existingDstFilesFn` in `internal/fs/copy_move.go`:

```go
dstObjs, err := listDst(ctx, dstPath)
if err != nil && !errors.Is(err, errs.ObjectNotFound) {
    return nil, errors.WithMessagef(err, "failed list dst [%s] objs", dstPath)
}
// non-existent dst → empty map, merge proceeds and creates dst on demand
```

A previous BFS-style "precreate one level of subdirectories" optimization was removed (2026-05-17, commit `6b3ce577`); directory creation now happens on demand via `op.Put`'s internal `MakeDir(parent)` and `op.MakeDir`'s recursive parent walk. The top-level dst `MakeDir` at `copy_move.go:198` is kept for early failure detection. 12 unit tests on `existingDstFilesFn` (including raw + wrapped `ObjectNotFound`) lock in the contract.

---

## HybridCache and Prefetch

See [streaming-and-caching.md](streaming-and-caching.md) for the full HybridCache description and `hybridSectionReader` prefetch details, which directly affect upload throughput for multipart drivers.
