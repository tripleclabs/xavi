# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
# Build the binary
go build -o xavi ./cmd/xavi

# Run all tests
go test ./...

# Run tests for a specific package
go test ./internal/agent/
go test ./internal/config/

# Run a single test
go test -run TestMergeAppConfig ./internal/agent/

# Cross-compile for Linux (typical deployment target)
GOOS=linux GOARCH=amd64 go build -o xavi-linux-amd64 ./cmd/xavi
GOOS=linux GOARCH=arm64 go build -o xavi-linux-arm64 ./cmd/xavi
```

## Architecture

Xavi is an edge infrastructure agent that manages Docker containers (Postgres, Valkey, App, Caddy, BackupBot) and forms a distributed gossip mesh with other Xavi nodes using `hashicorp/memberlist`.

### Package Layout

- **`cmd/xavi/main.go`** — Entry point. Parses `--auth` (base64 config bundle) and `--config-dir`/`-c` flags, sets up signal handling, creates and runs the Agent.
- **`internal/agent/`** — Core orchestrator. The `Agent` struct owns config, secrets, Docker client, config watcher, and cluster node. `Run()` is the main loop: loads config, watches for file changes, polls for updates. `applyConfig()` triggers `ensureInfrastructure()` which reconciles all containers.
- **`internal/container/`** — Docker client wrapper. `RunContainer()` implements convergence: inspects existing container, compares config (image, cmd, env, mounts, ports) via `compareConfig()`, and only recreates on mismatch.
- **`internal/config/`** — Config structs (`Config`, loaded from `/etc/tripleclabs/xavi.json`), `ParseBundle()` for base64 CLI bootstrap, and `Watcher` that polls file mtime for hot-reload.
- **`internal/cluster/`** — Gossip-based service discovery via `hashicorp/memberlist`. Nodes broadcast their enabled services and Postgres role (primary/secondary) as metadata. `FindServiceAddr()` and `FindPrimary()` discover remote nodes.
- **`internal/secrets/`** — Auto-generates and persists secrets (Postgres password, Valkey password, cluster encryption key, app encryption key, token secrets) to `xavi.secrets` with `0600` permissions. Uses `LoadOrGenerate` pattern: loads existing file, backfills any missing fields, saves.

### Key Patterns

- **Convergence loop**: The agent reconciles desired state on every config change and every 30s tick. Container client compares running container config against desired and only recreates on divergence.
- **Service discovery**: Each node broadcasts its `services` list and `pg_role` via gossip metadata. The app container gets `POSTGRES_HOST`/`VALKEY_HOST` pointed to either a local container name (e.g., `xavi-postgres`) or a remote node IP discovered via cluster.
- **Config merging**: App config (`pulse.json`) is a template that gets Postgres URL, Valkey URL, encryption key, and token secrets injected at `/tmp/xavi-app-config.json` before mounting into the app container.
- **All containers** are on a shared Docker bridge network (`xavi-net`) with `RestartPolicyAlways`.

### Container Names

All managed containers use the `xavi-` prefix: `xavi-postgres`, `xavi-valkey`, `xavi-app`, `xavi-caddy`, `xavi-backupbot`.
