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

## Config

`enabled`, `key` (shared secret), `label` (this node's display name), `addr`
(this node's public URL — advertise to be auto-discovered; leave empty behind
NAT), `seeds` (optional bootstrap URLs), `apply_remote` (adopt peer creds;
default on), `announce_interval_sec`. `active() == enabled && key != ""` — a
reachable node can run accept-only with no seed.

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
