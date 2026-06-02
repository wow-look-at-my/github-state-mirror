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
2. **Periodic refresh** — Every 6 hours, the service token's known resources are re-fetched as a backstop
3. **Lazy fetch** — On first access (or cache miss), data is fetched on demand before responding

Because high-frequency events are applied in place, an active org's cache no longer gets invalidated (and fully re-fetched) on every CI run or push — which is the point: serve from local state, only call GitHub on a genuine miss.

The SQLite database is a **cache**, not a database of record. On schema changes, the DB file is automatically deleted and recreated.

## API Endpoints

### Authentication

Every data endpoint **requires** a GitHub token — a personal access token or a GitHub App user-to-server token both work. Send it as a bearer token:

```
Authorization: Bearer <token>
```

Requests with no token are rejected with `401 Unauthorized` — they are never served using the server's own credentials. The token is validated against GitHub (`GET /user`) on first use and the result is cached in memory.

The only endpoint that does **not** require a bearer token is `POST /webhook`, which is authenticated by its GitHub HMAC signature instead (see `WEBHOOK_SECRET`).

### Data isolation

This service is designed to be safe to expose to multiple, mutually-untrusting callers. All cached data is partitioned by a **fingerprint of the caller's token** (a SHA-256 hash; the raw token is never stored or logged), not by GitHub username. The consequences:

- A caller can only ever read data that *their* token fetched. There is no code path by which one token observes another token's cached data.
- Because the partition is the token — not the login — even two tokens belonging to the *same* GitHub user are isolated. A read-only token granted to a third-party app cannot read private data that a broader personal token previously cached.
- On a cache miss the data is fetched using the caller's own token, so GitHub's authorization is always applied at fetch time. Staleness after an access change is bounded by each resource's TTL (and webhook invalidation), after which the next fetch re-authorizes.

### REST

- `GET /user` — authenticated user info (login, avatar)
- `GET /user/orgs` — user's organization list
- `GET /repos/{owner}/{repo}/compare/{base}...{head}` — ahead_by, behind_by
- `GET /repos/{owner}/{repo}/pulls/{number}/files` — changed files (path, additions, deletions)

### GraphQL

- `POST /graphql` — accepts org data queries, returns cached repo + PR data assembled from SQLite

### Webhook

- `POST /webhook` — receives GitHub webhook events, triggers cache invalidation

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `GITHUB_TOKEN` | No | — | Service token used **only** for background (periodic) refreshes, in its own credential partition. It is never used to serve API requests, which always require the caller's own `Authorization` header. |
| `WEBHOOK_SECRET` | For `/webhook` | — | HMAC secret for webhook signature verification. If unset, `POST /webhook` fails closed and rejects every delivery. |
| `LISTEN_ADDR` | No | `:8080` | HTTP listen address |
| `DB_PATH` | No | `github-mirror.db` | SQLite database file path |

## Building

```sh
go-toolchain
```

Binary is output to `build/server_linux_amd64`.

## Architecture

```
internal/
  actor/         Context-based per-credential identity propagation
  freshness/     Generic cache freshness framework (no GitHub knowledge)
  database/      SQLite schema + sqlc-generated queries
  ghdata/        Domain store (wraps sqlc with transactions)
  ghclient/      GitHub REST + GraphQL client (token→login cache)
  sync/          Fetchers, periodic refresh, webhook dispatch
  webhook/       HTTP handler, event parsing, HMAC verification
  api/           chi router, REST handlers, GraphQL assembler
```

The key design constraints: `internal/freshness/` is a generic cache-coherence engine that knows nothing about GitHub. The `internal/sync/` package bridges the freshness framework with GitHub-specific data. All cached data is scoped per-actor — where an actor is a fingerprint of the caller's token — to prevent cross-credential data leakage (see **Data isolation** above).
