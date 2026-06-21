# OpenList Backend (OP) — CLAUDE.md

Primary development guide for the Go backend. See [../INDEX.MD](../INDEX.MD) for the full subsystem→code map.

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
| `OpenList-Frontend/` | [Ironboxplus/OpenList-Frontend](https://github.com/Ironboxplus/OpenList-Frontend) | `main` | origin (upstream), ironbox (fork) |
| `movi-player/` | Local fork of [MrUjjwalG/movi-player](https://github.com/MrUjjwalG/movi-player) | `main` | — |
| `115-sdk-go/` | [Ironboxplus/115-sdk-go](https://github.com/Ironboxplus/115-sdk-go) | — | — |

### Module Replacements (`go.mod`)

| Module | Replace Target | Notes |
|--------|---------------|-------|
| `github.com/OpenListTeam/115-sdk-go` | `github.com/Ironboxplus/115-sdk-go v0.2.9` | Refresh context isolation, token error code filters, ErrDataEmpty, FlexString CID |
| `github.com/ProtonMail/go-proton-api` | `github.com/henrybear327/go-proton-api v1.0.0` | Community fork |
| `github.com/cronokirby/saferith` | `github.com/Da3zKi7/saferith v0.33.0-fixed` | Bug fix fork |

---

## Core Development Principles

1. **最小代码改动原则** (Minimum code changes): Make the smallest change necessary to achieve the goal
2. **不缓存整个文件原则** (No full file caching for seekable streams): For SeekableStream, use RangeRead instead of caching entire file
3. **必要情况下可以多遍上传原则** (Multi-pass upload when necessary): If rapid upload fails, fall back to normal upload

## Build and Development Commands

```bash
# Development
go run main.go                    # Run backend server (default port 5244)
air                              # Hot reload during development
./build.sh dev                   # Build dev version with frontend
./build.sh release               # Build release version

# Testing
go test ./...                    # Run all tests
go test ./drivers/115_open/ -v   # Run tests for a specific driver
go build ./drivers/115_open/...  # Quick compile check for a package

# Docker
docker-compose up && docker build -f Dockerfile .
```

- Requires Go 1.24+ (CI uses 1.25.0)
- `build.sh` fetches frontend from `$FRONTEND_REPO` (default: `Ironboxplus/OpenList-Frontend`) and embeds into `public/dist/`

## Key Naming Conventions

- Drivers: lowercase with underscores (`baidu_netdisk`, `aliyundrive_open`)
- Packages: lowercase (`internal/op`)
- Interfaces: PascalCase (`Reader`, `Writer`)
- Logging: `log.Infof("[driver_name] message")` via `logrus`
- Retries: `github.com/avast/retry-go`; HTTP default timeout 30s (`base.RestyClient`)

---

## Documentation Map

| Document | Content |
|----------|---------|
| [../INDEX.MD](../INDEX.MD) | Subsystem → deep-dive doc → code path index |
| [../OVERVIEW.md](../OVERVIEW.md) | High-level system map |
| [../JOURNAL.md](../JOURNAL.md) | Chronological change log (source of truth) |
| [../PLAN.md](../PLAN.md) | Active development roadmap |
| [docs/architecture.md](docs/architecture.md) | Driver system, request flow, internal packages, startup, common patterns |
| [docs/streaming-and-caching.md](docs/streaming-and-caching.md) | Link types, cache system, RangeReader, SeekableStream pitfalls, HybridCache |
| [docs/uploads.md](docs/uploads.md) | OSS callback validation, Put vs PutResult, merge-task ObjectNotFound tolerance |
| [docs/plugins.md](docs/plugins.md) | Frontend plugin registry/slots + backend yaegi (Go-source) runtime |
| [docs/frontend.md](docs/frontend.md) | Frontend dist serving, movi-player, subtitle handling |
| [docs/new-apis-2026-06-21.md](docs/new-apis-2026-06-21.md) | Storage loading progress, permission bit 16, video_play, favorites, driver_name |
