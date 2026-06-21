# github-state-mirror

A Go service that mirrors GitHub state into a local SQLite database, providing fast API access with automatic cache management.

## How It Works

Data stays fresh via three mechanisms:

1. **Webhooks (primary)** — GitHub pushes events and we apply the payload straight to the cache, with **no re-fetch**:
   - `pull_request` / `pull_request_review` → upsert the PR (and delete it on close)
   - `status` / `check_run` / `check_suite` → record the per-check state and recompute the commit-status rollup onto any PR whose head is that commit, and onto the repo's default-branch status when the check ran on the default branch
   - `push` → update the repo's `pushed_at`
   - `label` → recolor/remove the label across the repo's cached PRs

   Only low-frequency structural events (`repository`, `organization`, `membership`) — and any payload that can't be parsed — fall back to marking the affected resource stale for lazy refresh.

   A webhook is applied to every credential partition that has the affected repo cached. If **no** partition has it yet, the dispatcher **pulls it on demand** — fetching the owner's repos once, as the GitHub App installation named in the delivery, into that installation's partition — and then applies. So the first delivery for a repo bootstraps a scope itself; only when no app is configured (or the fetch fails) does a delivery fall through to `skipped` ("no cached scope").
2. **Periodic refresh** — When a GitHub App is configured (see Configuration), every 6 hours the service signs in as the app — per installation, never using a static service token — and re-fetches the resources already cached in each installation's partition as a backstop. (It does not pre-enumerate anything; partitions are populated on demand by lazy fetch and the webhook pull above.)
3. **Lazy fetch** — On first access (or cache miss), data is fetched on demand before responding

Because high-frequency events are applied in place, an active org's cache no longer gets invalidated (and fully re-fetched) on every CI run or push — which is the point: serve from local state, only call GitHub on a genuine miss.

The SQLite database is a **cache**, not a database of record. On schema changes, the DB file is automatically deleted and recreated.

## API Endpoints

### Authentication

Every data endpoint **requires** a GitHub token — a personal access token or a GitHub App user-to-server token both work. Send it as a bearer token:

```
Authorization: Bearer <token>
```

Requests with no token are rejected with `401 Unauthorized` — they are never served using the service's own credentials (the GitHub App used for background refreshes). The token is validated against GitHub (`GET /user`) on first use and the result is cached in memory.

The only endpoint that does **not** require a bearer token is `POST /webhook`, which is authenticated by its GitHub HMAC signature instead (see `WEBHOOK_SECRET`).

### Data isolation

This service is designed to be safe to expose to multiple, mutually-untrusting callers. All cached data is partitioned by a **fingerprint of the caller's token** (a SHA-256 hash; the raw token is never stored or logged), not by GitHub username. The consequences:

- A caller can only ever read data that *their* token fetched. There is no code path by which one token observes another token's cached data.
- Because the partition is the token — not the login — even two tokens belonging to the *same* GitHub user are isolated. A read-only token granted to a third-party app cannot read private data that a broader personal token previously cached.
- On a cache miss the data is fetched using the caller's own token, so GitHub's authorization is always applied at fetch time. Staleness after an access change is bounded by each resource's TTL (and webhook invalidation), after which the next fetch re-authorizes.

#### App-identity partition (for trusted first-party app callers)

A **GitHub App backend** whose installation tokens rotate hourly would get a fresh, empty fingerprint bucket every hour. Such a caller may instead assert a stable identity by sending its **GitHub App JWT** in an `X-Mirror-Identity` header. The mirror verifies that JWT against GitHub (`GET /app` — unforgeable, since only the app's private key produces a JWT GitHub accepts) and partitions the caller as `app:<id>`, so all of the app's rotating tokens share **one** warm, webhook-fed bucket. The `Authorization` token is still used for upstream fetches, so per-repo authorization is preserved. Callers that send no identity header keep the fingerprint isolation above unchanged — this mode is opt-in and additive. It is appropriate **only** for a first-party app (within an app's bucket, any of that app's tokens reads what the bucket cached), never as a way to relax the default isolation.

### REST

The mirror serves **no** REST endpoint from cache. It used to cache `/user`,
`/user/orgs`, `/repos/{owner}/{repo}/compare/{base}...{head}`, and
`/repos/{owner}/{repo}/pulls/{number}/files`, but with *trimmed* response shapes
(a subset of GitHub's fields). **A cache must be byte-for-byte identical to the
origin — not a transformative middleman** — so those routes were removed; they
now **pass through** to GitHub verbatim (see below). New cached REST endpoints
will be added back only when they return GitHub's exact shape, each gated by an
identity test.

### GraphQL

- `POST /graphql` — the org-repos query (an `organization { repositories { ... pullRequests ... } }` shape) is served from the cache, assembled from SQLite **to match exactly the fields GitHub returns for that query's selection set** (validated by an identity test against a recorded GitHub response). Any other GraphQL query is forwarded to GitHub (see passthrough below).

### GitHub passthrough (everything else)

Any request the mirror does not serve from cache is **transparently forwarded to GitHub** (`https://api.github.com`) and returned **uncached**. This makes the mirror a drop-in replacement for `api.github.com`: the endpoints above are served fast from the per-credential cache, and every other endpoint (`/rate_limit`, `/repos/{owner}/{repo}`, issues, releases, an unrecognized GraphQL query, ...) still works.

- The caller's `Authorization` header is forwarded unchanged — the mirror never substitutes its own `GITHUB_TOKEN` — and a forwarded request **still requires a token** (`401` otherwise), so the mirror is never an open, unauthenticated relay.
- Responses are passed through verbatim, including status, body, and headers such as `Link` (pagination) and `X-RateLimit-*`. The mirror's own CORS headers are authoritative; GitHub's duplicate `Access-Control-Allow-*` are stripped while `Access-Control-Expose-Headers` is preserved so browsers can read those rate-limit/link headers.
- This path is uncached: it never reads or writes the freshness store.

### OAuth token-exchange relay

- `POST /login/oauth/access_token` — relays a GitHub OAuth "exchange code for token" request to `github.com` and returns the response with the mirror's CORS headers. A purely client-side app (e.g. the repo-nightmare PR viewer) cannot call GitHub's token endpoint directly because it sends no CORS headers; the mirror stands in as the CORS-correct relay. It carries **no** bearer token (the OAuth `client_secret` in the body is the credential), so it is unauthenticated, and it targets `github.com` — not the `api.github.com` passthrough.

### Webhook

- `POST /webhook` — receives GitHub webhook events and applies them to the cache. The handler processes each delivery **synchronously** (the cache writes are small, idempotent upserts that finish well within GitHub's delivery deadline) and the HTTP response reflects what happened, so a "successful" delivery actually means data was preserved:
  - `200 OK` — applied: webhook data was written to one or more cache scopes
  - `202 Accepted` — received but nothing applied (no scope had the repo cached, an untracked event, or a fallback invalidation)
  - `500` — an internal error; GitHub retries (and the re-applied upsert is idempotent)

  The disposition and detail are returned in the JSON body and an `X-GSM-Disposition` header, and every delivery is recorded in the dashboard's webhook log (see below).

## Web Dashboard

Visit the service root (e.g. `https://github-state-mirror.pazer.io/`) and **sign in with GitHub** to see the state of the cache for your account: how many repos, pull requests, orgs, etc. are cached, the freshness of each resource kind (fresh / stale / fetching / error), and recent refresh activity.

- **What you see is yours.** The dashboard groups cache scopes by GitHub login. You only ever see scopes that *your own* tokens populated (a user may hold several tokens, each its own scope). This is a read-only view of counts and freshness metadata — it never exposes another credential's cached rows.
- **Admins see everything.** Logins listed in `ADMIN_LOGINS` (default `PazerOP`) get an **All scopes** view: per-scope stats across every cache partition, including the GitHub App's background-refresh partitions and any scope without a recorded identity. They also get two global, admin-only activity logs: a **Requests** tab — live data-API traffic and how each request was served (`hit` from cache / `miss` then fetched / `passthrough` to GitHub uncached), so you can see at a glance how much the cache is actually used vs. proxied through — and a **Webhooks** tab — recent webhook deliveries and each one's disposition (`applied` / `skipped` / `invalidated` / `ignored` / `error`), confirming whether incoming events update the cache. Both span every repo, so they are admin-only.
- **Admins can browse and audit each scope.** Every scope (in both the **My cache** and **All scopes** views) gets two admin-only actions:
  - **Browse** opens the actual cached rows for that scope — repositories, open pull requests with their labels and CI status, orgs, users, PR files, branch comparisons, and commit checks — plus a copyable raw-JSON dump of everything cached. This is a read-only window onto what is already stored; the data tables stay keyed by the opaque fingerprint.
  - **Check** runs a **consistency check**: it re-fetches the source of truth from GitHub (as the mirror's own GitHub App) and emits a JSON diff of any drift — repos/PRs that are only in the cache or only on GitHub, and field mismatches such as a stale `last_commit_status`, `default_branch_status`, draft flag, or labels. The report is designed to be copied and handed to a tool/assistant for analysis. It needs a configured GitHub App (the same one used for background refresh); without one the action reports "unavailable". Owners the app is not installed on are listed as skipped rather than reported as missing.
- **Separate from the data API.** Dashboard authorization is by GitHub login (an OAuth session cookie), distinct from the data API's per-token fingerprint model. The fingerprint→login mapping (`actor_identities`) exists purely so the UI can attribute scopes; it does **not** relax data isolation — the data tables remain keyed by the opaque token fingerprint.

Dashboard routes (session-cookie auth, not bearer tokens):

- `GET /` — the dashboard page
- `GET /login` → `GET /auth/callback` — GitHub OAuth sign-in
- `POST /logout` — clear the session
- `GET /api/me` — `{ authenticated, login_configured, login, is_admin }`
- `GET /api/cache?scope=mine|all` — cache stats for the signed-in user (`mine`) or every scope (`all`, admin only)
- `GET /api/requests` — recent data-API requests and their cache disposition (hit/miss/passthrough), plus per-disposition totals (admin only; in-memory, resets on restart)
- `GET /api/webhooks` — recent webhook deliveries and their dispositions (admin only)
- `GET /api/cache/data?actor=<fingerprint>` — the actual cached rows for one scope, as flattened JSON (admin only)
- `GET /api/cache/check?actor=<fingerprint>[&org=<owner>]` — consistency-check diff of one scope against GitHub's live state (admin only; requires a configured GitHub App)

Sign-in requires a GitHub OAuth App; set `GITHUB_OAUTH_CLIENT_ID` / `GITHUB_OAUTH_CLIENT_SECRET` (see below). With those unset the page still renders but the sign-in button is disabled.

## Configuration

The service has **no static service token**. API requests authenticate with the caller's own bearer token; the only credential the service itself uses is an optional GitHub App, exclusively for background work — periodic refreshes and pulling an as-yet-uncached repo on demand when a webhook arrives for it.

| Variable | Required | Default | Description |
|---|---|---|---|
| `GITHUB_APP_ID` | No | — | GitHub App ID. When set (together with a private key), the service signs in as this app for background work — periodic refreshes and the webhook dispatcher's on-demand repo pulls — minting a short-lived access token per installation. The app's data lives in its own credential partition and is **never** served to API callers, who always authenticate with their own `Authorization` header. Leave unset to disable periodic refreshes and on-demand pulls (webhooks for uncached repos then skip). |
| `GITHUB_APP_PRIVATE_KEY` | With `GITHUB_APP_ID` | — | The app's PEM private key (PKCS#1 or PKCS#8). May be `\n`-escaped onto a single line for convenience in env vars. |
| `GITHUB_APP_PRIVATE_KEY_PATH` | Alt. to inline | — | Path to a PEM private-key file. Takes precedence over `GITHUB_APP_PRIVATE_KEY`. |
| `WEBHOOK_SECRET` | For `/webhook` | — | HMAC secret for webhook signature verification. If unset, `POST /webhook` fails closed and rejects every delivery. |
| `LISTEN_ADDR` | No | `:8080` | HTTP listen address |
| `DB_PATH` | No | `github-mirror.db` | SQLite database file path |
| `ALLOWED_ORIGINS` | No | `*` | Comma-separated CORS allow-list for browser clients. Safe to leave open because data is isolated by token fingerprint, not origin. |
| `GITHUB_OAUTH_CLIENT_ID` | For dashboard login | — | GitHub OAuth App client ID. Register the app's callback URL as `<BASE_URL>/auth/callback`. |
| `GITHUB_OAUTH_CLIENT_SECRET` | For dashboard login | — | GitHub OAuth App client secret. |
| `SESSION_SECRET` | Recommended | random per-process | HMAC key for signed session cookies. If unset, a random key is generated at startup, so sessions reset on restart. Set it in production. |
| `ADMIN_LOGINS` | No | `PazerOP` | Comma-separated GitHub logins granted the **All scopes** dashboard view (case-insensitive). |
| `BASE_URL` | No | derived from request | Public base URL (e.g. `https://github-state-mirror.pazer.io`) used to build the OAuth `redirect_uri`. Derived from the request (honoring `X-Forwarded-Proto`) when unset. |

## Building

```sh
go-toolchain
```

Binary is output to `build/server_linux_amd64`.

The dashboard front-end is authored in TypeScript under `internal/api/web/src/` and compiled to `internal/api/web/assets/*.js`, which is **committed** (a generated artifact) and embedded into the binary via `//go:embed`. After editing the `.ts` sources, regenerate the JS:

```sh
npm ci        # first time only
npm run build # tsc: src/*.ts -> assets/*.js
```

CI's `web-check` job fails if the committed JS is out of date with the TypeScript source (run `npm run build` and commit). A `preview` job deploys a standalone, backend-free styling preview of the dashboard to buildhost for each branch, served at `https://sites.pazer.build/github-state-mirror/branch/<branch>/`.

## Docker

A container image is published to the GitHub Container Registry on every push to `master`:

```
ghcr.io/wow-look-at-my/github-state-mirror:latest
```

It is a static binary on a `distroless` base (no shell, runs as a non-root user), listens on port `8080`, and keeps its SQLite cache under `/var/lib/github-state-mirror` (declared as a volume), so `DB_PATH` defaults to `/var/lib/github-state-mirror/github-mirror.db` inside the container.

```sh
docker run -p 8080:8080 \
  -e GITHUB_APP_ID=123456 \
  -e GITHUB_APP_PRIVATE_KEY_PATH=/etc/github-app.pem \
  -e WEBHOOK_SECRET=... \
  -v "$PWD/github-app.pem:/etc/github-app.pem:ro" \
  -v github-state-mirror-data:/var/lib/github-state-mirror \
  ghcr.io/wow-look-at-my/github-state-mirror:latest
```

The GitHub App is optional — omit `GITHUB_APP_ID` and the key to run without background refreshes (per-request data and webhooks still work).

The SQLite database is a disposable cache, so persisting it with a volume is optional. The image is built and pushed by the `publish-ghcr` job in `.github/workflows/ci.yml`, which reuses `wow-look-at-my/actions/.github/workflows/publish-ghcr.yml` (downloads the CI build artifact, builds the `Dockerfile`, pushes to GHCR, and prunes old versions).

## Architecture

```
internal/
  actor/         Context-based per-credential identity propagation
  freshness/     Generic cache freshness framework (no GitHub knowledge)
  database/      SQLite schema + sqlc-generated queries
  ghdata/        Domain store (wraps sqlc with transactions)
  ghclient/      GitHub REST + GraphQL client (token→login cache)
  auth/          GitHub OAuth login + signed-cookie sessions (dashboard)
  sync/          Fetchers, periodic refresh, webhook dispatch
  webhook/       HTTP handler, event parsing, HMAC verification
  api/           chi router, REST handlers, GraphQL assembler, web dashboard
  api/web/       Dashboard front-end (TypeScript src/ -> embedded assets/)
```

The key design constraints: `internal/freshness/` is a generic cache-coherence engine that knows nothing about GitHub. The `internal/sync/` package bridges the freshness framework with GitHub-specific data. All cached data is scoped per-actor — where an actor is a fingerprint of the caller's token — to prevent cross-credential data leakage (see **Data isolation** above).
