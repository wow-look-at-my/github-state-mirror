# GitHub State Mirror

## Project Overview

A Go service that mirrors GitHub state into SQLite, providing a fast local API surface. This is a **cache** — the SQLite DB is disposable and gets nuked+recreated on schema version changes.

## Architecture

- `internal/actor/` — Context-based actor (per-credential) identity propagation (stdlib-only, safe to import from anywhere)
- `internal/freshness/` — Generic cache freshness framework (zero GitHub knowledge)
- `internal/database/` — SQLite schema + sqlc-generated queries (`dbgen/` is codegen, do not edit)
- `internal/ghdata/` — Domain store wrapping sqlc with transaction logic (`dashboard.go` = cross-actor cache stats)
- `internal/ghclient/` — GitHub REST + GraphQL API client (includes in-memory token→login cache)
- `internal/auth/` — GitHub OAuth login + signed-cookie sessions for the dashboard (stdlib-only, no DB knowledge)
- `internal/sync/` — Bridge layer: concrete fetchers, periodic refresh, webhook dispatch
- `internal/webhook/` — Webhook HTTP handler + event parsing
- `internal/api/` — chi router, REST handlers, GraphQL cache assembler, the GitHub passthrough proxy (`proxy.go`), and the web dashboard (`dashboard.go` + embedded `web/`)
- `internal/api/web/` — Dashboard front-end: `src/*.ts` (TypeScript source) compiled to `assets/*.js` (committed, generated, embedded)
- `cmd/server/` — Entry point

## Key Constraints

- **No migrations** — bump `SchemaVersion` in `internal/database/db.go` to nuke+recreate
- **sqlc codegen** — run `sqlc generate` after modifying `schema.sql` or `queries/*.sql`
- **Freshness/data separation** — `internal/freshness/` must never import GitHub-specific packages
- **Per-credential cache isolation** — all cached data is scoped by actor, where the actor is a SHA-256 **fingerprint of the caller's token** (`ghclient.Fingerprint`), set in `requireAuth` (`internal/api/router.go`). Any valid GitHub token (PAT or App user-to-server) is accepted; isolation is by token, not token type. This is the security boundary: a credential only ever reads what it fetched, so the service is safe for untrusting multi-tenant use. The `actor` column is part of every table's primary key. Data endpoints reject tokenless requests with 401; the `GITHUB_TOKEN` is used **only** by the background refresher, in its own fingerprint partition, never to serve requests. Webhooks invalidate/apply across all actors via `MarkStaleByKindKey` / `ActorsForRepo` (only for credentials that already cached the repo).
- **Webhook-fed cache (the whole point)** — the dispatcher (`internal/sync/webhook.go`) applies payloads **directly** to the cache so high-frequency events never trigger a GitHub re-fetch: `pull_request`/`pull_request_review` upsert the PR; `status`/`check_run`/`check_suite` aggregate per-check state in the `commit_checks` table and roll it up onto each PR's `last_commit_status` (and the repo's `default_branch_status` when the check ran on the default branch, matched by `head_branch`/`branches` vs the payload's `default_branch`); `push` updates `pushed_at`; `label` recolors/removes labels. Invalidation (`MarkStaleByKindKey`) is only a **fallback** for structural events (`repository`/`organization`/`membership`) or unparseable payloads. Do NOT regress this into invalidate-and-refetch — leveraging pushed webhook state to avoid upstream fetches is the entire purpose of the project. `UpsertPullRequest` COALESCEs `last_commit_status` so a PR webhook (which carries no CI state) doesn't clobber a status set by a check webhook.
- **GitHub passthrough for unknown endpoints** — the mirror is a drop-in for `api.github.com`: only the handful of endpoints in `router.go` are served from the per-credential cache; everything else is transparently reverse-proxied to GitHub, **uncached**, via `newGitHubProxy` (`internal/api/proxy.go`). It is wired as chi's `NotFound`/`MethodNotAllowed` (so the `Use` middleware — CORS, recoverer — still wraps it), and the GraphQL handler forwards any query that is not the cached org-repos shape. The proxy forwards the caller's `Authorization` verbatim (never the server `GITHUB_TOKEN`) and rejects tokenless requests with 401 so the mirror is not an open relay. It targets `gh.BaseURL()` (so tests hit the fake upstream). The mirror's `corsMiddleware` is the single CORS authority: `ModifyResponse` strips GitHub's `Access-Control-Allow-*` (a duplicate `Access-Control-Allow-Origin` breaks browsers) but keeps `Access-Control-Expose-Headers`. Do NOT make this path touch the freshness store — it must stay uncached.
- **Dashboard = separate authz model** — the web dashboard (`internal/api/dashboard.go`, served at `/`) authenticates a human via **GitHub OAuth** and authorizes by **login** (session cookie), which is deliberately distinct from the data API's bearer-token + fingerprint model. It never serves one credential's cached rows to another — it only reports per-scope **counts + freshness metadata**. A user sees the scopes their own tokens populated; logins in `ADMIN_LOGINS` (default `PazerOP`) see all scopes. The `actor_identities` table maps fingerprint→login for this grouping only; it does NOT relax data isolation (data tables stay keyed by the opaque fingerprint — do NOT switch the data partition to a username/hash, or a narrow token would read what a broad token cached). Identity rows are written (debounced) in `requireAuth`.
- **TypeScript front-end** — the dashboard JS is authored in `internal/api/web/src/*.ts` and compiled by `npm run build` (tsc) to `internal/api/web/assets/*.js`, which is **committed as a generated artifact** and embedded via `//go:embed`. Edit the `.ts`, never the `.js`. CI's `web-check` job fails if the committed JS is stale (run `npm run build` and commit). `assets/demo-data.js` is preview-only (NOT embedded); the CI `preview` job injects it to deploy a backend-free styling preview to buildhost per branch.
- **Build** — use `go-toolchain` (not `go build`/`go test` directly); run `npm run build` after editing `web/src/*.ts`

## Commands

- `go-toolchain` — runs mod tidy, vet, test, build (all-in-one)
- `sqlc generate` — regenerates `internal/database/dbgen/*.gen.go` (after editing `schema.sql` or `queries/*.sql`)
- `npm run build` — compiles `internal/api/web/src/*.ts` → `internal/api/web/assets/*.js` (after editing the dashboard front-end)

## Environment Variables

- `GITHUB_TOKEN` (optional) — service token for background (periodic) refreshes only; never used to serve API requests, which require the caller's own `Authorization` header (401 otherwise)
- `WEBHOOK_SECRET` — GitHub webhook HMAC secret
- `LISTEN_ADDR` — HTTP listen address (default `:8080`)
- `DB_PATH` — SQLite database file path (default `github-mirror.db`)
- `ALLOWED_ORIGINS` (optional) — comma-separated CORS allow-list for browser clients (e.g. the repo-nightmare PR viewer). Defaults to `*` (any origin), which is safe because data is isolated by token fingerprint, not origin. Preflight `OPTIONS` is answered without auth; see `internal/api/cors.go`.
- `GITHUB_OAUTH_CLIENT_ID` / `GITHUB_OAUTH_CLIENT_SECRET` (optional) — GitHub OAuth App credentials for dashboard login. If unset, the dashboard still renders but sign-in is disabled. Register the OAuth App's callback as `<BASE_URL>/auth/callback`.
- `SESSION_SECRET` (optional) — HMAC key for session cookies. If unset, a random per-process key is used (sessions reset on restart); set it in production.
- `ADMIN_LOGINS` (optional) — comma-separated logins that may view **all** cache scopes (default `PazerOP`); case-insensitive.
- `BASE_URL` (optional) — public base URL (e.g. `https://github-state-mirror.pazer.io`) used to build the OAuth `redirect_uri`; derived from the request (honoring `X-Forwarded-Proto`) when unset.
