# github-state-mirror

A Go service that mirrors GitHub state into a local SQLite database, providing fast API access with automatic cache management.

## How It Works

Data stays fresh via three mechanisms:

1. **Webhooks** — GitHub pushes events, the mirror invalidates + refreshes affected resources immediately
2. **Periodic refresh** — Every 6 hours, all known resources are re-fetched as a fallback
3. **Lazy fetch** — On first access (or cache miss), data is fetched on demand before responding

The SQLite database is a **cache**, not a database of record. On schema changes, the DB file is automatically deleted and recreated.

## API Endpoints

### Authentication

All API requests pass through the caller's `Authorization` header to GitHub. Send your token the same way you would to the GitHub API:

```
Authorization: Bearer ghp_xxxx
```

Each caller's GitHub username is resolved on first request (via `GET /user`) and cached in memory. All cached data is scoped per-user — one user's private data is never visible to another user's requests.

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
| `GITHUB_TOKEN` | No | — | Fallback GitHub token for background refreshes; API requests pass through the caller's `Authorization` header |
| `WEBHOOK_SECRET` | No | — | HMAC secret for webhook signature verification |
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
  actor/         Context-based per-user identity propagation
  freshness/     Generic cache freshness framework (no GitHub knowledge)
  database/      SQLite schema + sqlc-generated queries
  ghdata/        Domain store (wraps sqlc with transactions)
  ghclient/      GitHub REST + GraphQL client (token→login cache)
  sync/          Fetchers, periodic refresh, webhook dispatch
  webhook/       HTTP handler, event parsing, HMAC verification
  api/           chi router, REST handlers, GraphQL assembler
```

The key design constraints: `internal/freshness/` is a generic cache-coherence engine that knows nothing about GitHub. The `internal/sync/` package bridges the freshness framework with GitHub-specific data. All cached data is scoped per-actor (GitHub username) to prevent cross-user data leakage.
