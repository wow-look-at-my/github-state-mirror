# GitHub State Mirror

## Project Overview

A Go service that mirrors GitHub state into SQLite, providing a fast local API surface. This is a **cache** — the SQLite DB is disposable and gets nuked+recreated on schema version changes.

## Architecture

- `internal/actor/` — Context-based actor (per-credential) identity propagation (stdlib-only, safe to import from anywhere)
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
- **Per-credential cache isolation** — all cached data is scoped by actor, where the actor is a SHA-256 **fingerprint of the caller's token** (`ghclient.Fingerprint`), set in `requireAuth` (`internal/api/router.go`). Any valid GitHub token (PAT or App user-to-server) is accepted; isolation is by token, not token type. This is the security boundary: a credential only ever reads what it fetched, so the service is safe for untrusting multi-tenant use. The `actor` column is part of every table's primary key. Data endpoints reject tokenless requests with 401; the `GITHUB_TOKEN` is used **only** by the background refresher, in its own fingerprint partition, never to serve requests. Webhooks invalidate/apply across all actors via `MarkStaleByKindKey` / `ActorsForRepo` (only for credentials that already cached the repo).
- **Webhook-fed cache (the whole point)** — the dispatcher (`internal/sync/webhook.go`) applies payloads **directly** to the cache so high-frequency events never trigger a GitHub re-fetch: `pull_request`/`pull_request_review` upsert the PR; `status`/`check_run`/`check_suite` aggregate per-check state in the `commit_checks` table and roll it up onto each PR's `last_commit_status` (and the repo's `default_branch_status` when the check ran on the default branch, matched by `head_branch`/`branches` vs the payload's `default_branch`); `push` updates `pushed_at`; `label` recolors/removes labels. Invalidation (`MarkStaleByKindKey`) is only a **fallback** for structural events (`repository`/`organization`/`membership`) or unparseable payloads. Do NOT regress this into invalidate-and-refetch — leveraging pushed webhook state to avoid upstream fetches is the entire purpose of the project. `UpsertPullRequest` COALESCEs `last_commit_status` so a PR webhook (which carries no CI state) doesn't clobber a status set by a check webhook.
- **Build** — use `go-toolchain` (not `go build`/`go test` directly)

## Commands

- `go-toolchain` — runs mod tidy, vet, test, build (all-in-one)
- `sqlc generate` — regenerates `internal/database/dbgen/*.gen.go`

## Environment Variables

- `GITHUB_TOKEN` (optional) — service token for background (periodic) refreshes only; never used to serve API requests, which require the caller's own `Authorization` header (401 otherwise)
- `WEBHOOK_SECRET` — GitHub webhook HMAC secret
- `LISTEN_ADDR` — HTTP listen address (default `:8080`)
- `DB_PATH` — SQLite database file path (default `github-mirror.db`)
- `ALLOWED_ORIGINS` (optional) — comma-separated CORS allow-list for browser clients (e.g. the repo-nightmare PR viewer). Defaults to `*` (any origin), which is safe because data is isolated by token fingerprint, not origin. Preflight `OPTIONS` is answered without auth; see `internal/api/cors.go`.
