# Cluster credential sync

A self-contained plugin (`internal/cluster`) that lets multiple OpenList nodes
share **storage credentials** (tokens / cookies / secrets) in real time, so when
one node refreshes a token the others adopt it automatically. Disabled by
default; configured per-node under **Manage → Plugins → Cluster credential
sharing**.

The design satisfies three hard requirements from the product owner:

1. **Sync ONLY the credential** — never the whole storage config. Mount path,
   root folder, cache, order, proxy and every other field stay local to each
   node. ("除了 key 其他的都不用同步")
2. **Manual, multipartite pairing** — the admin explicitly groups the storages
   (across any number of nodes) that represent the same account. A group is a
   connected component of a k-partite graph. ("手动选择 storage … 多分图")
3. **Auto-discovery** — no hand-maintained peer list. Any node that authenticates
   with the shared key joins; reachable nodes advertise an address and the rest
   of the mesh is learned automatically. ("远程连过来，认证过了就加入 peer")

## Layers

| File | Responsibility |
|------|----------------|
| `crypto.go` | PSK → HKDF-SHA256 → AES-256-GCM; ed25519 identity (`node_id = base32(sha256(pubkey)[:16])`); sign/verify |
| `identity.go` | Persist a 32-byte seed at `<data>/cluster/identity.key` (0600) |
| `creds.go` | Extract / overlay / hash the **credential subset** of a driver's Addition |
| `state.go` | CRDTs: sync-group document, per-group credential records, node inventory |
| `transport.go` | `syncMessage` wire form + AEAD `envelope` + replay cache |
| `conn.go` | Persistent WebSocket peer connections, registry, dial supervisor, PEX dialing |
| `engine.go` | `Manager`: lifecycle, storage hook, push/pull/apply, relay, anti-entropy, events |
| `status.go` | Redaction-safe overview (nodes/groups/connections/events/stats) for the UI |
| `config.go` | Per-node `Config` (`enabled/key/label/addr/seeds/apply_remote`) + atomic store |

## What a "credential" is

OpenList drivers do not tag credential fields, so `creds.go` identifies them by
name over the Addition JSON keys: any key whose normalized name contains
`token, cookie, password, passwd, secret, auth, session, refresh, access,
credential, apikey, appkey, privatekey, signkey, ticket, passport`. Structural
fields (`root_folder_id`, `order_by`, …) never match, so they are never shared.
`applyCreds` overlays only those keys onto a peer's Addition and reports whether
anything actually changed — identical credentials cause no write, no re-init, no
churn. `credHash` (sha256 of the canonicalized credential map) is the
idempotency key.

## Sync groups

`group{ id, name, members[]{ node_id, mount_path } }`. The whole set is a
**single LWW document** (`groupDoc`) — signed by its author, versioned by a
Lamport clock, tie-broken by origin id. Admin edits are rare, so whole-doc LWW is
simple and convergent. The document is gossiped to every node; `pruneCreds`
drops credential records for groups that no longer exist.

## Credential records

Per group, `credRecord{ group_id, origin_driver, fields[], cred_hash, payload,
version, origin, origin_pub, sig }`. The secret `payload` only ever travels
inside the AEAD-sealed envelope. Records are signed by their origin so they stay
authentic across relays. Merge is LWW by `(version, origin, cred_hash)`, and
idempotent: an identical `cred_hash` is ignored regardless of version.

### Flow

1. A driver refreshes its token → `op.MustSaveDriverStorage` → `callStorageHooks("update")`.
2. `onStorageHook` finds the groups containing `(this_node, mount)`. If the
   storage is healthy (`status == work`) it extracts the credential, and for each
   group `localCredChange` authors a new record (skipped if the hash is
   unchanged) and broadcasts a `push`. If the storage is **broken**
   (`status != work`) or a driver fired `token-invalid`, it does **not** push —
   it `pullGroup`s to recover a good credential from peers instead. ("坏 token 不推")
3. A peer receives the record, `verify`s the signature, `mergeCred`s under LWW,
   then `applyCredRecord` overlays the payload onto its own member mounts
   (`op.UpdateStorage`, which re-inits the driver with the new token). The
   resulting `update` hook is a no-op because the credential now matches the
   record's hash — no echo, no loop.
4. The record is `relay`ed to other connections, so two NAT'd nodes converge
   through a common reachable peer.

Health gating ensures a broken token is never propagated; idempotency ensures a
node that already holds the same token does nothing. ("其他节点看到一样的 token 就不用变")

## Discovery & transport

The link is a persistent WebSocket at `/api/cluster/ws` (NAT-friendly: a node
behind NAT dials out and keeps the link open; a reachable node accepts inbound
and relays). Authentication is the PSK: any frame that opens the AEAD envelope
admits the sender as a peer.

- Each node advertises a `nodeInfo` inventory (its `label`, `addr`, and the
  `mount_path`/`driver`/`status` of every storage). Inventory is gossiped and
  powers the UI graph + group editor.
- **PEX**: nodes exchange the dialable `addr`s they know; the dial supervisor
  opens a connection to any advertised peer it is not already connected to. A
  `seed` URL is only needed to bootstrap the very first join.
- Per-frame freshness: timestamp window + nonce replay cache.

## API

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/api/cluster/ws` | Peer WebSocket endpoint (PSK-authenticated) |
| GET | `/api/admin/cluster/config` | Config (key redacted) + live status |
| POST | `/api/admin/cluster/config` | Save config (blank/`********` key keeps current) |
| GET | `/api/admin/cluster/status` | Live status only |
| POST | `/api/admin/cluster/groups` | Replace the sync-group document |
| POST | `/api/admin/cluster/refresher` | Set (empty node id: clear) a group's preferred refresher: `{group_id, node_id}` |

## Config

`enabled`, `key` (shared secret), `label` (this node's display name), `addr`
(this node's public URL — advertise to be auto-discovered; leave empty behind
NAT), `seeds` (optional bootstrap URLs), `apply_remote` (adopt peer creds;
default on), `announce_interval_sec`. `active() == enabled && key != ""` — a
reachable node can run accept-only with no seed.

## Single refresher (115 Open)

115's `refresh_token` is single-use: whoever refreshes invalidates every other
copy. So in a group of `115 Open` storages exactly one member spends it.

- **Lease.** Each group has a signed refresher lease `(epoch, seq, holder)`,
  gossiped like any other record. The newest `(epoch, seq, holder)` dominates;
  expiry is judged on the *receiver's* clock (`lease_ttl_sec`, default 60 s), so
  clock skew between nodes cannot extend a lease. The holder renews at TTL/6.
- **Who holds it.** The admin's preferred refresher if online, else the
  working member that is first by public address, then node id. Set it with
  `POST /api/admin/cluster/refresher` or the cluster page; an empty node id
  returns to automatic. A node that is preferred takes the lease back from a
  temporary holder (release to preferred).
- **Proactive refresh.** The holder refreshes at 80 % of `expires_in`, never
  blind: until the lifetime of the current access token is known (first
  refresh, or `expires_in` carried by the group record) it waits for a 401.
  Attempts per mount are at least `proactive_min_gap_sec` (30 s) apart.
- **Followers never refresh.** A follower whose access token is rejected pulls
  the group first; if nothing newer arrives within `refresh_request_wait_sec`
  (30 s) it sends the holder a *refresh-request* (directed message, relayed by
  a hub if needed, at most 5 per rejected token, 60 s apart). A follower never
  revokes a still-good refresh token because its *access* token lapsed.
- **Takeover.** When the lease has expired, members take over in rank order
  (preferred, working, address, node id; staggered by rank, with a startup
  grace). A taker pulls the latest credential **before** claiming and waits 2 s
  for a dominating simultaneous claim before it refreshes.
- **Stalled paths.** A node whose links are up but silent (no frame or pong
  recently) must not claim: a stale token spent on 115 kills the group. An
  isolated node waits 3x the TTL.
- **Delivery.** After a new version the holder watches the members' `announce`
  digests; a member that has not confirmed it within `delivery_resend_sec`
  (20 s) gets the credential resent directly (through a hub when needed).
- **Revocations.** A signed revocation digest rides on `announce`; a mismatch
  triggers a pull. Superseded revocations older than
  `revocation_retention_days` (14) are pruned, only behind a per-group
  UpdatedAt floor so a pruned revocation cannot be resurrected by a stale record.

### Transport

- Each peer has a **coalescing send state** (priority class for leases, acks and
  refresh-requests; bulk class for records), not a FIFO: a newer item for the
  same key replaces the pending one. A link is never closed because of backlog;
  it closes only on read/write errors, a write timeout or pong loss.
- One link per node pair: if both ends dial, the dial of the smaller node id
  wins (`keepNewLink` tie-break).
- Redial backoff 2 s .. 60 s with +-25 % jitter.
- `GET /api/admin/cluster/status` reports per link: connected, direction, RTT,
  queue depth, last anti-entropy, connects/disconnects, flaps in 10 min and the
  last disconnect reason; per group: refresher, lease epoch/expiry, preferred,
  last refresh (node, time, outcome, trigger, `expires_in`), recent
  refresh-requests, each member's held version (laggards flagged) and protocol
  level; `mixed` is set when a member predates leases.

### Logs

Every refresh is logged by the 115 driver (`[115_open] refresh ok|failed
mount=... trigger=proactive|refresh-request|recover|<code>`), and the cluster
logs each push, pull, apply, probe failure, revocation, lease claim/release/
takeover and refresh-request (`[cluster] ...`).

### Rollout and compatibility

Protocol level 2 adds `lease`, directed messages, `ack` and the revocation
digest as optional JSON fields / new message types. Old builds ignore unknown
fields and types, so a mixed cluster keeps working, with these limits:

- An **old node** (no leases) still refreshes on its own and can kill the
  group's token. The status marks it `legacy` and sets the group `mixed`.
- An **old hub** does not relay leases, directed messages or acks, so new nodes
  behind it cannot see each other's lease. Credentials still flow through it.
  Tested (BB5): two new nodes behind an old hub make one refresh and the other
  adopts it, but this is not safe in general.
- Therefore: **upgrade all nodes of a 115 group together, hubs (public nodes)
  first**, then restart. Do not run a long-lived mixed cluster.
- The cluster key rotation planned with this release needs all online nodes to
  restart with the new key together.

### Config (added)

`lease_ttl_sec` (60), `refresh_request_wait_sec` (30), `proactive_min_gap_sec`
(30), `delivery_resend_sec` (20), `revocation_retention_days` (14). All
optional; 0 means default.

### Tests

- Unit and in-process tests: `go test -tags emu115 ./internal/cluster/ ./drivers/115_open/ ./internal/op/`
  (without the tag the emulator-backed cluster tests skip; add `-race` with `CGO_ENABLED=1 CC=gcc`). They drive the real 115 driver and
  SDK against the local emulator `115-sdk-go/emu115` (a real HTTP server).
- Black-box: `go test -tags blackbox ./internal/cluster/blackbox/ -v` runs three
  real server processes on free ports under `E:\Go\Openlist\tmp\cluster-bb\`
  (override with `CLUSTER_BB_DIR`), with a fault TCP proxy between followers and
  the hub. It needs `bin-new.exe` (current tree, built with
  `go build -tags emu115 -buildvcs=false -o ...`) and, for BB5,
  `bin-old.exe` built from `c02580c4` (`CLUSTER_BB_BIN`, `CLUSTER_BB_OLD_BIN`).
  Only binaries built with `-tags emu115` read `OPENLIST_TEST_115_API_BASE` /
  `OPENLIST_TEST_115_PASSPORT_BASE` to point the 115 driver at the emulator.
  Release builds do not contain that code at all (a test asserts it), so an
  environment variable can never redirect real tokens to another host. Never
  ship a binary built with `-tags emu115`.

## State on disk

`<data>/cluster/identity.key` (seed), `config.json`, `state.json`
(groups + credential records + inventory + Lamport clock). State is written
atomically (temp + rename).

## Tests

`*_test.go` cover credential extraction / overlay (node-local fields preserved) /
idempotency, group-doc and credential-record LWW + signature forgery rejection,
inventory merge + dialable-address selection, envelope round-trip / replay /
stale / wrong-key, and `wsURL` / connection registry. A two-node integration run
(discovery → inventory → group propagation → credential overlay on token change →
events) is validated end to end.
