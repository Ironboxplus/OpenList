# Traffic statistics (`internal/stats`)

The admin dashboard shows how many bytes this node relayed and how fast.

## Where bytes are counted

The four global stream limiters in `internal/stream/limit.go` are the one place
every relayed byte passes. `bootstrap.initLimiter` wraps each of them once with
`stats.Wrap(kind, limiter)`; the wrapper adds `n` to a counter in `WaitN`, then
delegates. Drivers and call sites are unchanged. `SetLimit`/`SetBurst` are
forwarded, so the speed-limit settings keep working.

| kind | what passes it |
|---|---|
| `server_download` | data read from a storage: proxy playback, `/d` proxy, WebDAV/S3 proxy reads, sync downloads, SFTP/SMB reads |
| `server_upload` | data written to a storage by drivers that use `driver.NewLimitedUploadStream` / `NewLimitedUploadFile` / `ServerUploadLimitWaitN` |
| `client_download` | bytes sent to clients by the rate-limited HTTP/WebDAV download handlers and FTP |
| `client_upload` | bytes received from clients (limited upload handlers, FTP) |

## Counting rules

- A 302 redirect lets the client fetch from the storage directly: **not counted**.
- A rapid upload (hash match) moves no bytes: nothing to count.
- A retried read happens again and is counted again.
- A proxied download is in both `server_download` and `client_download`. The two
  groups are shown side by side and are never added together.
- Only `WaitN` counts. `Wait`, `AllowN` and `ReserveN` are forwarded untouched;
  nothing calls them on these limiters today.
- `stream.RateLimitReader` skips the limiter when a `Read` returns data together
  with an error (e.g. `io.EOF`), so that last chunk is not counted.
- Cluster nodes count only themselves; the dashboard names the node.

## Persistence

- Live speed: a one-second sampler keeps the last 300 points (5 minutes) in memory.
- Table `traffic_dailies (day, kind, bytes)`, primary key `(day, kind)`; `day` is
  the server's local `YYYY-MM-DD`.
- Every 60 s the counted delta is added with an upsert (`bytes = bytes + ?`),
  and once more on shutdown (`bootstrap.Release`, before the database closes).
- A failed save only logs; the delta stays pending and goes out with the next flush.
- A delta is booked on the day of the flush as a whole; a delta spanning
  midnight is not split.

## API

`GET /api/admin/stats/overview` (admin only): `node`, `traffic`
(`now_bps`, `series`, `today`, `month`, `total`, `daily` = last 30 days,
zero-filled), `media` (counts of movie/tv/anime/collection, episodes, unscraped).
`today`/`month`/`total` are the saved values plus the bytes not yet flushed.
`media` is all zeros while media is switched off, like `/api/media/library`.
Transfer and sync figures are not served here; use the transfer and sync APIs.
