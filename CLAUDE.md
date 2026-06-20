# GitHub State Mirror

## Project Overview

A Go service that mirrors GitHub state into SQLite, providing a fast local API surface. This is a **cache** — the SQLite DB is disposable and gets nuked+recreated on schema version changes.

## Architecture

- `internal/actor/` — Context-based actor (per-credential) identity propagation (stdlib-only, safe to import from anywhere)
- `internal/freshness/` — Generic cache freshness framework (zero GitHub knowledge)
- `internal/database/` — SQLite schema + sqlc-generated queries (`dbgen/` is codegen, do not edit)
- `internal/ghdata/` — Domain store wrapping sqlc with transaction logic (`dashboard.go` = cross-actor cache stats)
- `internal/ghclient/` — GitHub REST + GraphQL API client (includes in-memory token→login cache and `AppAuthenticator` for GitHub App JWT / installation-token minting)
- `internal/auth/` — GitHub OAuth login + signed-cookie sessions for the dashboard (stdlib-only, no DB knowledge)
- `internal/sync/` — Bridge layer: concrete fetchers, periodic refresh, webhook dispatch
- `internal/webhook/` — Webhook HTTP handler + event parsing
- `internal/api/` — chi router, REST handlers, GraphQL cache assembler, the passthrough proxy (`proxy.go`), and the web dashboard (`dashboard.go` + embedded `web/`)
- `internal/api/web/` — Dashboard front-end: `src/*.ts` (TypeScript source) compiled to `assets/*.js` (committed, generated, embedded)
- `cmd/server/` — Entry point

## Key Constraints

- **No migrations** — bump `SchemaVersion` in `internal/database/db.go` to nuke+recreate
- **sqlc codegen** — run `sqlc generate` after modifying `schema.sql` or `queries/*.sql`
- **Freshness/data separation** — `internal/freshness/` must never import GitHub-specific packages
- **Per-credential cache isolation** — all cached data is scoped by actor, where the actor is a SHA-256 **fingerprint of the caller's token** (`ghclient.Fingerprint`), set in `requireAuth` (`internal/api/router.go`). Any valid GitHub token (PAT or App user-to-server) is accepted; isolation is by token, not token type. This is the security boundary: a credential only ever reads what it fetched, so the service is safe for untrusting multi-tenant use. The `actor` column is part of every table's primary key. Data endpoints reject tokenless requests with 401. There is **no static service token**: the background refresher signs in as a **GitHub App** (`ghclient.AppAuthenticator`), minting a short-lived access token per installation, and runs in its own stable `app-installation:<id>` partition (see `sync.AppSessions`), never to serve requests. Webhooks invalidate/apply across all actors via `MarkStaleByKindKey` / `ActorsForRepo` (only for credentials that already cached the repo).
- **App-identity partition (opt-in, for trusted first-party app callers)** — a *request*-time caller may send a **GitHub App JWT** in the `X-Mirror-Identity` header. `requireAuth` verifies it via `ghclient.VerifyAppIdentity` (`GET /app` — GitHub only 200s if the RS256 signature checks out against the app's public key, so it's unforgeable; cached per-JWT) and partitions that caller as `app:<id>` instead of by token fingerprint. This is distinct from the background refresher's `app-installation:<id>` partitions above: that's the mirror signing in as *its own* app for periodic refreshes; this is an *external* app caller (e.g. pr-minder) tagging its data-API requests. It exists because such a caller's installation tokens rotate hourly — fingerprinting them would give a fresh empty bucket every hour; an app identity gives **one stable, webhook-fed bucket** for all the app's tokens. The `Authorization` token is still injected into the context for upstream fetches/passthrough, so per-repo authorization is unchanged. Security caveat: within an app's bucket, any of that app's tokens can read what the bucket cached (the reader is a single trusted codebase), so this is appropriate **only** for a first-party app, never as a way to relax the default fingerprint isolation. No identity header → unchanged fingerprint behavior.
- **Passthrough proxy (`internal/api/proxy.go`)** — the chi authed group ends with a `/*` catch-all (`handlers.passthrough`) that forwards any non-cached request verbatim to `api.github.com` (`ghclient.Forward`: same method/path/query/body, caller's token, response copied back minus hop-by-hop headers) and `slog.Warn`s the path. This makes the mirror a **drop-in GitHub API base URL** (the pr-minder integration points its base URL here): cached endpoints are served from SQLite, everything else (single-PR reads, branches, reviews, all writes, App-level JWT endpoints) reaches GitHub transparently. The identity header is **never** forwarded upstream. `POST /graphql` is the cache assembler (org read query only), NOT a GraphQL proxy — mutations/non-org queries must go to GitHub directly.
- **Webhook-fed cache (the whole point)** — the dispatcher (`internal/sync/webhook.go`) applies payloads **directly** to the cache so high-frequency events never trigger a GitHub re-fetch: `pull_request`/`pull_request_review` upsert the PR; `status`/`check_run`/`check_suite` aggregate per-check state in the `commit_checks` table and roll it up onto each PR's `last_commit_status` (and the repo's `default_branch_status` when the check ran on the default branch, matched by `head_branch`/`branches` vs the payload's `default_branch`); `push` updates `pushed_at`; `label` recolors/removes labels. Invalidation (`MarkStaleByKindKey`) is only a **fallback** for structural events (`repository`/`organization`/`membership`) or unparseable payloads. Do NOT regress this into invalidate-and-refetch — leveraging pushed webhook state to avoid upstream fetches is the entire purpose of the project. `UpsertPullRequest` COALESCEs `last_commit_status` so a PR webhook (which carries no CI state) doesn't clobber a status set by a check webhook.
- **Dashboard = separate authz model** — the web dashboard (`internal/api/dashboard.go`, served at `/`) authenticates a human via **GitHub OAuth** and authorizes by **login** (session cookie), which is deliberately distinct from the data API's bearer-token + fingerprint model. It never serves one credential's cached rows to another — it only reports per-scope **counts + freshness metadata**. A user sees the scopes their own tokens populated; logins in `ADMIN_LOGINS` (default `PazerOP`) see all scopes. The `actor_identities` table maps fingerprint→login for this grouping only; it does NOT relax data isolation (data tables stay keyed by the opaque fingerprint — do NOT switch the data partition to a username/hash, or a narrow token would read what a broad token cached). Identity rows are written (debounced) in `requireAuth`.
- **TypeScript front-end** — the dashboard JS is authored in `internal/api/web/src/*.ts` and compiled by `npm run build` (tsc) to `internal/api/web/assets/*.js`, which is **committed as a generated artifact** and embedded via `//go:embed`. Edit the `.ts`, never the `.js`. CI's `web-check` job fails if the committed JS is stale (run `npm run build` and commit). `assets/demo-data.js` is preview-only (NOT embedded); the CI `preview` job injects it to deploy a backend-free styling preview to buildhost per branch.
- **Build** — use `go-toolchain` (not `go build`/`go test` directly); run `npm run build` after editing `web/src/*.ts`

## Commands

- `go-toolchain` — runs mod tidy, vet, test, build (all-in-one)
- `sqlc generate` — regenerates `internal/database/dbgen/*.gen.go` (after editing `schema.sql` or `queries/*.sql`)
- `npm run build` — compiles `internal/api/web/src/*.ts` → `internal/api/web/assets/*.js` (after editing the dashboard front-end)

## Environment Variables

- `GITHUB_APP_ID` (optional) — GitHub App ID. When set with a private key, the service signs in as this app for background (periodic) refreshes only; never used to serve API requests, which require the caller's own `Authorization` header (401 otherwise). Unset = periodic refreshes disabled.
- `GITHUB_APP_PRIVATE_KEY` / `GITHUB_APP_PRIVATE_KEY_PATH` — the App's PEM private key (PKCS#1 or PKCS#8), inline (optionally `\n`-escaped) or as a file path (path wins). Required when `GITHUB_APP_ID` is set.
- `WEBHOOK_SECRET` — GitHub webhook HMAC secret
- `LISTEN_ADDR` — HTTP listen address (default `:8080`)
- `DB_PATH` — SQLite database file path (default `github-mirror.db`)
- `ALLOWED_ORIGINS` (optional) — comma-separated CORS allow-list for browser clients (e.g. the repo-nightmare PR viewer). Defaults to `*` (any origin), which is safe because data is isolated by token fingerprint, not origin. Preflight `OPTIONS` is answered without auth; see `internal/api/cors.go`.
- `GITHUB_OAUTH_CLIENT_ID` / `GITHUB_OAUTH_CLIENT_SECRET` (optional) — GitHub OAuth App credentials for dashboard login. If unset, the dashboard still renders but sign-in is disabled. Register the OAuth App's callback as `<BASE_URL>/auth/callback`.
- `SESSION_SECRET` (optional) — HMAC key for session cookies. If unset, a random per-process key is used (sessions reset on restart); set it in production.
- `ADMIN_LOGINS` (optional) — comma-separated logins that may view **all** cache scopes (default `PazerOP`); case-insensitive.
- `BASE_URL` (optional) — public base URL (e.g. `https://github-state-mirror.pazer.io`) used to build the OAuth `redirect_uri`; derived from the request (honoring `X-Forwarded-Proto`) when unset.
