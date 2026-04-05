# GitHub State Mirror

## Project Overview

A Go service that mirrors GitHub state into SQLite, providing a fast local API surface. This is a **cache** — the SQLite DB is disposable and gets nuked+recreated on schema version changes.

## Architecture

- `internal/actor/` — Context-based actor (user) identity propagation (stdlib-only, safe to import from anywhere)
- `internal/freshness/` — Generic cache freshness framework (zero GitHub knowledge)
- `internal/database/` — SQLite schema + sqlc-generated queries (`dbgen/` is codegen, do not edit)
- `internal/ghdata/` — Domain store wrapping sqlc with transaction logic
- `internal/ghclient/` — GitHub REST + GraphQL API client (includes in-memory token→login cache)
- `internal/sync/` — Bridge layer: concrete fetchers, periodic refresh, webhook dispatch
- `internal/webhook/` — Webhook HTTP handler + event parsing
- `internal/api/` — chi router, REST handlers, GraphQL cache assembler
- `cmd/server/` — Entry point

## Key Constraints

- **No migrations** — bump `SchemaVersion` in `internal/database/db.go` to nuke+recreate
- **sqlc codegen** — run `sqlc generate` after modifying `schema.sql` or `queries/*.sql`
- **Freshness/data separation** — `internal/freshness/` must never import GitHub-specific packages
- **Per-actor cache isolation** — all cached data is scoped by actor (GitHub username). The `actor` column is part of every table's primary key. Webhooks invalidate across all actors via `MarkStaleByKindKey`.
- **Build** — use `go-toolchain` (not `go build`/`go test` directly)

## Commands

- `go-toolchain` — runs mod tidy, vet, test, build (all-in-one)
- `sqlc generate` — regenerates `internal/database/dbgen/*.gen.go`

## Environment Variables

- `GITHUB_TOKEN` (optional) — fallback GitHub token for background refreshes; API requests pass through the caller's `Authorization` header
- `WEBHOOK_SECRET` — GitHub webhook HMAC secret
- `LISTEN_ADDR` — HTTP listen address (default `:8080`)
- `DB_PATH` — SQLite database file path (default `github-mirror.db`)
