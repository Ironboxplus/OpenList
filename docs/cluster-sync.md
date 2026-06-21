# Cluster Storage Config Sync — Deep Dive

Package: `internal/cluster`
Status: implemented, integration test in progress (2026-06-21)
Journal entry: [2026-06-21 — Cluster Storage Config Sync Plugin](../../JOURNAL.md)

---

## Overview

The cluster sync plugin lets multiple OpenList nodes securely share storage driver
configurations in real time. Key properties:

- **Cryptographically authenticated** — pre-shared key (PSK) + per-node ed25519 identity
- **NAT-friendly** — nodes behind NAT dial out; a persistent WebSocket holds the NAT mapping
- **CRDT-consistent** — Last-Writer-Wins, idempotent, tombstone deletes, no churn on identical content
- **Health-gated** — broken / expired tokens never leave the originating node

---

## Package Layout

```text
internal/cluster/
  crypto.go          HKDF key derivation, AES-256-GCM seal/open, ed25519 identity
  identity.go        Seed persistence (loadOrCreateIdentity)
  state.go           CRDT store: syncableStorage, record, digest, merge
  config.go          Config struct, configStore (atomic save), active/peerList/shouldShare
  transport.go       syncMessage, envelope, sealEnvelope/openEnvelope, replayCache
  conn.go            peerConn, connRegistry, dialPeer, ServeWS, dialSupervisor, ensureDial
  engine.go          Manager (Default singleton), Init/Start/Stop, hook wiring, relay logic
  *_test.go          Unit tests — all passing
```

---

## Cryptographic Design

### Key Derivation

```text
PSK (user-supplied string)
  └─► HKDF-SHA256
        salt = "openlist-cluster"
        info = "openlist-cluster-aead-v1"
        length = 32 bytes
  └─► AES-256-GCM AEAD key
```

HKDF provides domain separation — the same PSK could be re-used with different `info`
strings for future sub-keys without collisions.

### Wire Frame

Every message is wrapped in a sealed envelope:

```text
[nonce: 12 bytes][GCM ciphertext + tag][AAD: frame type byte]
```

- `sealEnvelope(key, plaintext, aad)` → `envelope{Nonce, Ciphertext}`
- `openEnvelope(key, env, aad)` → plaintext or error
- Only PSK holders can seal or open — unauthenticated nodes see only opaque bytes.

### Replay Protection

`replayCache` tracks seen nonces within a sliding time window. A frame is rejected if:

- its nonce has been seen before (replay), or
- its embedded timestamp falls outside the acceptance window (stale / future frame).

### Node Identity

Each node generates an ed25519 keypair on first start. The 32-byte seed is persisted at
`<data>/cluster/identity.key` (mode 0600).

```text
seed (32 B, persisted)
  └─► ed25519 keypair (newIdentity / identityFromSeed)

node_id = base32( sha256(pubkey)[:16] )   — 16 uppercase characters
```

Sync records are self-authenticating:

```text
record.OriginPub  — sender's public key (32 bytes)
record.Sig        — ed25519 signature over content hash
record.verify()   checks:
  1. nodeIDFromPub(OriginPub) == record.Origin
  2. sha256(canonical JSON) matches embedded ContentHash
  3. ed25519.Verify(OriginPub, ContentHash, Sig)
```

A node that does not know the PSK cannot open envelopes. A node that knows the PSK but
forges a record will fail `record.verify()` because it cannot sign with the origin node's
private key.

---

## CRDT State Machine

### Keying Strategy

The CRDT is keyed by **content hash** — a SHA-256 of the storage configuration's canonical
JSON with identity-mutable fields excluded:

```text
excluded: ID, Status, Modified
included: driver name, mount path, all driver-specific config fields
```

Consequences:

- Two storages with identical config but different IDs share the same hash — idempotent,
  no churn.
- A token refresh changes the config → new hash → a real LWW update is generated and
  propagated.
- Renaming a mount path changes the hash — treated as delete + create (tombstone + new
  record).

### Record Lifecycle

```text
localChange(storage)
  ├─ health check: skip if storage.Status == StatusBroken or token appears invalid
  ├─ compute contentHash
  ├─ if store already has same hash at same-or-higher Lamport clock → no-op (idempotent)
  └─ create signed record, bump Lamport clock, insert into store

localDelete(storage)
  └─ create tombstone record (Deleted=true), signed, bump clock, insert

merge(remote record)
  ├─ verify signature
  ├─ if local has same contentHash at higher-or-equal clock → discard (dominates)
  ├─ if tombstone + local has live record at lower clock → apply tombstone
  └─ otherwise apply: insert/update store entry
```

`record.dominates(other)` implements the LWW tiebreak: higher Lamport clock wins; equal
clocks tiebreak by node_id lexicographic order (deterministic, no coin flip needed).

### Persistence

`state.json` in `<data>/cluster/` is written atomically (tmp file + rename) on every
change. `snapshot()` serialises the full store. `load()` re-hydrates on startup, so
nodes survive restarts without losing sync state.

---

## Transport Layer

### Design Choice: Persistent WebSocket over Stateless HTTP

Stateless HTTP POST was considered and rejected: it cannot reach nodes behind NAT because
the caller must initiate — a node behind NAT has no publicly reachable address for peers
to POST to.

**Persistent WebSocket** solves this: the NAT'd node dials out, the TCP connection is
held open, and the remote peer can push frames back at any time over the same connection.
Ping keepalives prevent the NAT mapping from timing out.

### Connection Model

```text
┌──────────────┐                        ┌──────────────┐
│  NAT'd node  │ ── WS dial ──────────► │ Reachable B  │
│              │ ◄─────────── push/relay ─              │
└──────────────┘                        └──────┬───────┘
                                               │ relay
                                        ┌──────▼───────┐
                                        │ Reachable C  │
                                        └──────────────┘
```

- `dialPeer(url)` — dials and blocks until the connection closes, then returns so
  `dialSupervisor` can retry with exponential backoff.
- `ServeWS(c *gin.Context)` — upgrades an HTTP request to WebSocket and registers the
  connection in `connRegistry`.
- `ensureDial(peer)` — idempotent: if a dial goroutine is already running for `peer`,
  does nothing.
- `relay(records, excludeConn)` — after `applyRecords` returns the set of newly applied
  records, `relay` forwards them to all other connected peers (excluding the sender to
  prevent echo).

### Keepalive Constants

| Constant | Value | Purpose |
|---|---|---|
| `pingPeriod` | 54 s | How often the server sends a WS ping frame |
| `pongWait` | 60 s | How long to wait for a pong before closing |
| `writeWait` | 10 s | Deadline for a single write operation |
| `dialBackoffMin` | 2 s | Initial retry delay after dial failure |
| `dialBackoffMax` | 5 min | Maximum retry delay (exponential cap) |
| `sendQueueLen` | 128 | Per-connection outbound channel depth |
| `maxFrameBytes` | 16 MB | Maximum inbound frame size |

### Connection Registry

`connRegistry` is a goroutine-safe map of active `*peerConn` entries, keyed by connection
ID. A node_id is bound to a connection once identity is established during the WS
handshake (`bind`). `hasNode(nodeID)` lets the engine avoid duplicate dials.

### URL Normalisation

`wsURL(rawURL, path)` converts HTTP base URLs to WebSocket URLs:

```text
http://host/...   →  ws://host/api/cluster/ws
https://host/...  →  wss://host/api/cluster/ws
```

---

## Engine (Manager)

`Manager` is the central coordinator. `Default` is the package-level singleton initialised
by `InitClusterSync()` in `internal/bootstrap/cluster.go`.

### Lifecycle

```text
InitClusterSync()           called during Init(), after InitPlugins()
  └─ Manager.Init(dataDir)
       ├─ loadOrCreateIdentity
       ├─ load configStore
       └─ load state.json (if present)

StartClusterSync()          called during Start(), after LoadStorages()
  └─ Manager.Start(ctx)
       ├─ seedLocalStorages()    — feed local storages into CRDT on first start
       ├─ announceLoop()         — periodic full-state announce to all peers
       ├─ dialSupervisor()       — maintain outbound WS connections to configured peers
       └─ register storage hooks
```

### Storage Hooks

Two hooks wire the engine into the storage lifecycle:

| Hook event | Source | Engine action |
|---|---|---|
| `"update"` | `saveDriverStorage` (after token refresh) | `onStorageHook` → `localChange` → broadcast |
| `"token-invalid"` | `NotifyStorageTokenInvalid` | `onStorageHook` → skip propagation (health gate) |

The hook fires as a goroutine (`go callStorageHooks(...)`) so it never blocks the caller's
hot path.

### Announce Loop

Every `AnnounceIntervalSec` (default 30 s) the engine broadcasts a digest summary of its
CRDT store to all peers. Peers that have diverged request missing records; peers that are
up to date discard the digest without generating traffic. This provides eventual consistency
for nodes that were offline during a change.

### Apply + Relay

```go
func (m *Manager) applyRecords(records []record) []record
```

- Verifies each record's signature.
- Calls `store.merge()` for each.
- Returns the subset of records that were actually applied (i.e., advanced the CRDT state).
- The caller (`handleFrame`) passes the applied set to `relay()`.

---

## API Endpoints

Registered in `server/router.go`:

| Method | Path | Handler | Auth |
|---|---|---|---|
| `GET` | `/api/cluster/ws` | `ClusterWS` | API key (peer-to-peer) |
| `GET` | `/api/admin/cluster/config` | `ClusterGetConfig` | Admin |
| `POST` | `/api/admin/cluster/config` | `ClusterSetConfig` | Admin |
| `GET` | `/api/admin/cluster/status` | `ClusterStatus` | Admin |

`ClusterSetConfig` calls `Manager.SetConfig` which atomically saves the new config and
calls `connRegistry.closeAll()` — existing connections drop and `dialSupervisor` reconnects
with the new peer list.

---

## Configuration Reference

Persisted at `<data>/cluster/config.json`. Editable via the admin UI or directly as JSON.

```json
{
  "enabled": true,
  "key": "<pre-shared key>",
  "peers": ["https://peer-b.example.com"],
  "share_drivers": [],
  "share_mounts": ["/cluster-test"],
  "share_deletes": false,
  "apply_remote": true,
  "announce_interval_sec": 30,
  "request_timeout_sec": 10
}
```

| Field | Type | Meaning |
|---|---|---|
| `enabled` | bool | Master switch — `false` disables all sync activity |
| `key` | string | Pre-shared key; all cluster members must use the same value |
| `peers` | `[]string` | Base URLs of peer nodes this node dials out to |
| `share_drivers` | `[]string` | Only share storages using these driver names (empty = all) |
| `share_mounts` | `[]string` | Only share storages whose mount path matches a prefix (empty = all) |
| `share_deletes` | bool | Whether to propagate tombstone (delete) records |
| `apply_remote` | bool | Whether to apply incoming records to the local database |
| `announce_interval_sec` | int | Seconds between periodic full-state announce broadcasts |
| `request_timeout_sec` | int | HTTP / WS dial timeout |

`active()`, `peerList()`, and `shouldShare()` are **pointer receivers** on `*Config` to
ensure callers always read the live configuration after a `SetConfig` call.

---

## Frontend UI (`ClusterConfig.tsx`)

Located at `src/pages/manage/plugins/ClusterConfig.tsx`, rendered inside the Plugins
management page.

Sections:

1. **Enable switch** — master on/off toggle
2. **Pre-shared Key** — password field (masked)
3. **Peers** — textarea, one URL per line
4. **Scope** — `share_drivers` (comma-separated), `share_mounts` (comma-separated)
5. **Behaviour** — `apply_remote`, `share_deletes` switches; interval number inputs
6. **Live Status** — polls `/api/admin/cluster/status`; shows `node_id`, connected peer
   count, and number of synced records

---

## Operational Notes

### Data Files

```text
<data>/cluster/
  identity.key    32-byte ed25519 seed (mode 0600, never share)
  config.json     cluster configuration
  state.json      CRDT store snapshot (rebuilt on startup if missing)
```

### Safe Scoping for Testing

Set `share_mounts` to a single test path (e.g. `/cluster-test`) so that production
storages are never touched by the sync mechanism.

### NAT'd Node Setup

A NAT'd node only needs to list reachable peers in `peers`. It dials out on startup and
after each disconnect. It does not need an open inbound port.

### Token Refresh Propagation

When `saveDriverStorage` is called after a token refresh, the `"update"` hook fires
asynchronously. The engine calls `localChange` (health-checked) and, if the content hash
changed, broadcasts to all connected peers. Peers receiving the update call `applyToLocal`
→ `op.UpdateStorage`, which persists the fresh token without triggering another hook cycle
(the content hash will be identical on the receiving side after apply).

### Avoiding Churn

The idempotency guarantee means that if node A and node B both store the same token
(same content hash), neither will generate a CRDT update for the other. The announce
digest handshake confirms they agree; no record is transmitted.

### Broken Token Gate

`localChange` checks storage health before accepting a record into the CRDT store. If the
storage status is `StatusBroken`, or if the token fields appear invalid, the record is
silently dropped. This prevents a node that has entered a broken-token state from
poisoning healthy peers.

---

## Testing

| File | Coverage |
|---|---|
| `crypto_test.go` | AEAD round-trip, wrong key, tampered AAD, identity binding |
| `state_test.go` | LWW merge, idempotency (same content hash), tombstone, Lamport ordering |
| `config_test.go` | `active()` / `peerList()` pointer-receiver correctness, `shouldShare()` filter |
| `transport_test.go` | Envelope replay rejection, stale-timestamp rejection |
| `conn_test.go` | `wsURL` conversion (http→ws, https→wss), `connRegistry` add/bind/remove/closeAll |

All tests pass as of rev `2c8a9827`.

---

## Related Docs

- [streaming-and-caching.md](streaming-and-caching.md) — RangeReader, SeekableStream pitfalls
- [new-apis-2026-06-21.md](new-apis-2026-06-21.md) — storage loading progress, permission bit 16, video_play, favorites
- [architecture.md](architecture.md) — driver system, request flow, packages, startup sequence
