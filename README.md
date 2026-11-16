# github-state-mirror

A Go service that mirrors GitHub state into a local SQLite database, providing fast API access with automatic cache management.

## How It Works

Data stays fresh via multiple mechanisms:

1. **Webhooks (primary)** — GitHub pushes events and we apply the payload straight to the cache, with **no re-fetch**:
   - `pull_request` / `pull_request_review` → upsert the PR (and delete it on close)
   - `status` / `check_run` / `check_suite` → record the per-check state and recompute the commit-status rollup onto any PR whose head is that commit. This is onto the repo's default-branch status when the check ran on the default branch
   - `push` → update the repo's `pushed_at`
   - `label` → recolor/remove the label across the repo's cached PRs

   - `repository` → lifecycle: a deleted repo is removed (cascade). A renamed one moves, privatized/publicized updates the stored visibility

   Only a structural payload that cannot be parsed falls back to marking the affected resource stale for lazy refresh.

   **Webhooks always apply.** There is ONE global truth store, so a delivery never asks "who has this cached?". An event for a repo nobody has ever fetched upserts the repo row straight from the payload's own `repository` object and applies. There is no "skipped" outcome and no on-demand fetch on the webhook path.
2. **Periodic fleet refresh** — When a GitHub App is configured (see Configuration), immediately at startup and then every few hours (`REFRESH_INTERVAL`) the service signs. This is in as the app. This is per installation, not using a static service token — and syncs **each installation's account**: every repo + open PR of that owner. This is Organizations and Users alike (an owner-agnostic `repositoryOwner` GraphQL query). Only repos the account actually **owns** are synced (`ownerAffiliations: OWNER`, plus a client-side guard that drops — and logs — any foreign-owner node): a User's *collaborator* repos belong to their real owners and are never keyed under the user. Each sync lands in global truth and earns the installation's stable `app-installation:<id>` principal its grants. A brand-new installation is synced on the first cycle (the refresher names the owner itself — it does not wait for a pre-existing freshness marker). **Lost deliveries are recovered.** GitHub never re-sends a delivery it can not hand over, so every few minutes the service reads the App's own failure log and asks. This is for the missing ones (`WEBHOOK_REPLAY_INTERVAL`). See [docs/webhooks/delivery-gaps.md](docs/webhooks/delivery-gaps.md).
3. **Lazy fetch** — On first access (or cache miss), data is fetched on demand — with the caller's own token — before responding

Because high-frequency events are applied in place, an active org's cache no longer gets invalidated (and fully re-fetched) on every CI run or push. This is the point: serve from local state, only call GitHub on a genuine miss.

The SQLite database is a **cache**, not a database of record. On schema changes, the DB file is automatically deleted and recreated.

## API Endpoints

[docs/api-endpoints.md](docs/api-endpoints.md) holds this section.

## Web Dashboard

Visit the service root (e.g. `https://github-state-mirror.pazer.io/`) and **sign in with GitHub** to see the state of the cache. The global truth totals (repos, pull requests, commit checks, contents, git commits, grants — one cache, one truth), your principal's reveal-layer standing (how many repos you hold live grants for), org-sync freshness, and recent refresh activity.

- **Truth is shared. Grants are yours.** The totals are the global store's counts — the same numbers for every viewer. What is per-you is the show layer: your principal's live grant count and sync freshness. The dashboard never exposes cached rows to non-admins.
- **Admins see everything.** Logins listed in `ADMIN_LOGINS` (default `PazerOP`) get a **Principals** view: every known principal with its login, live grant count, and last-seen time. This is plus a per-principal **Grants** action showing exactly which repos the show layer will serve it (and whether each grant came from a `list_sync` or a `probe`). They also get global activity logs: a **Requests** tab.

  Live data-API traffic and how each request was served (`hit` from cache / `miss` then fetched / `passthrough` read forwarded uncached / `write` mutation proxied), plus grouped tables above the flat history — **Top requests** and **Top uncached requests**. This aggregats traffic by route shape (e.g. `/repos/{owner}/{repo}/compare/{basehead}`) since start.

  As a result, the hottest routes and the hottest uncached ones (the caching candidates) are visible at a glance (in-memory like the rest of the request log) — and a **Webhooks** tab. Recent webhook deliveries and each one's disposition (`applied` / `invalidated` / `ignored` / `error`. There is no "skipped" under the global model), confirming that incoming events update truth.

  A **Rate limit** tab shows GitHub rate-limit standing ways: **live** — a `GET /rate_limit` poll per installation of the mirror's own GitHub App (the credential behind background refreshes and the consistency check). This is **observed** — the `X-RateLimit-*` headers GitHub attaches to every response, passively recorded off each upstream path (passthrough proxy, cache-miss fetches, show probes, the client's own calls). This is grouped per identity (principal or credential-derived label. Never a raw token) and resource bucket.

  The observed half costs zero API calls and covers the callers' own credentials, not the App. Zero-usage readings (nothing consumed in the current window, e.g. only 304s) are hidden as noise. It is in-memory like the request log and resets on start. Without a GitHub App the tab still works, showing observed data plus a live polling is unavailable.

  Both tabs resolve principals to their **verified display names** — a user's login, an app's slug, an installation's account login. This is learned from verification calls that already happen — showing e.g. `pr-minder` instead of a bare `app:3433933` (the key stays visible. This is in a tooltip/caption. Unverified labels such as token fingerprints keep the bare key).

  A **Timeline** tab renders **every exchange the mirror participates in** as a live swimlane chart (the `<timeline-view>` canvas component, consumed at runtime from js-snippets' buildhost library site) — a gap on this chart is a bug. Nothing is deliberately hidden. On it, each with its **real measured duration**: every incoming webhook delivery try, verified or not (`⇐ push`, `⇐ pull_request`, …. Rejected/unverified deliveries on the fixed `⇐ (unverified)` lane — untrusted headers never mint lanes).

  Every inbound data-API request the mirror serves, timed end-to-end — cache hits, misses, passthroughs, writes, errors — one lane per route shape (`GET /repos/{owner}/{repo}/pulls`, `POST /graphql`, …). The mirror→GitHub leg of every cached-route miss (`upstream`) and every show probe (`probe`). Every call the mirror's own GitHub client makes. Identity resolution, app verification, token mints, fleet-refresh and consistency-check GraphQL, rate-limit polls — one event per real try (`internal`). The github.com login relays (`relay`).

  And every outbound subscriber-notification try (`⇒ notify`, with status/try/last). The uncharted surface, stated openly: the dashboard's own UI endpoints — the chart polling itself will recursively fill the chart with the act of viewing it. Failures (rejected deliveries, dispatch errors, upstream 5xx, failed notify attempts) are emphasized. `ignored` deliveries render dim. Ms-scale events render as instant pips at wide zooms and re-promote to bars zoomed in. Tooltips carry the delivery/actor/try detail.

  The backing ring keeps the **last many hours** (plus a 100k-event cap) in memory — like the request log, a live view that resets on start.
- **Admins can browse and audit global truth.** The Principals tab carries truth-wide actions:
  - **Browse truth** opens the actual cached rows. Repositories (with stored visibility), open pull requests with their labels and CI status, and commit checks — plus a copyable raw-JSON dump.
  - **Run consistency check** re-fetches the source of truth from GitHub (as the mirror's own GitHub App, via the owner-agnostic `repositoryOwner` query — User-account installations are checked like Organizations) and emits a JSON diff of any drift between GLOBAL truth. This is GitHub — repos/PRs only in the cache or only on GitHub.

    Field mismatches such as a stale `last_commit_status`, `default_branch_status`, `visibility`, `is_archived`, `pushed_at` (5-minute tolerance. Lag implies missed push webhooks), `auto_merge_method`, draft flag, or labels (`mergeable` is deliberately not compared: the cache un-resolves it on pushes). A repo cached **public** that GitHub says is private/internal gets its own `visibility_leak` issue (the show fast path is serving it). A cached-only repo that is **archived** is classified as expected (own tally) rather than drift.

    Cached-open PRs under a missing repo are swept as their own entries. PR-existence entries carry `served_now` (a live pulls-list marker means the incorrect list is being served right now) and every discrepancy carries a short `fix` hint. A missing repo that is **private** on GitHub is marked and tallied separately (`repos_only_on_github_private`). Under lazy truth it simply has not been absorbed yet, which is expected, not a failure.

    The report includes `truth_freshness`: per owner, the most recent org list-sync any principal ran. It needs a configured GitHub App. Without one the action reports "unavailable". Owners the app is not installed on are listed as skipped rather than reported as missing.
  - **Reconcile** runs the same check and then **corrects** the drift from the same fetched snapshot (`POST /api/cache/check?apply=true`, behind a confirmation). Missing repos/open PRs are absorbed into truth (grants recorded under the installation principal), stale cached-open PRs are deleted, `visibility` / `default_branch_status` / `auto_merge_method` are set from GitHub's answers **including nulls**. The COALESCE upserts can never write, and a poisoned CI rollup (a ghost PENDING `commit_checks` row whose completion delivery was missed) is fixed by deleting the contradicted rows and setting GitHub's verdict. This is a correction that survives the next PR webhook. The response is the normal report plus an `applied` tally per action. A plain GET stays strictly read-only. Both actions stream live NDJSON progress (`?stream=1`) so the modal shows a per-owner, per-page progress bar instead of sitting silent for the minutes. A fleet run takes.
- **Separate from the data API.** Dashboard authorization is by GitHub login (an OAuth session cookie), distinct from the data API's bearer-token + show model. The principal→display-name mapping (`actor_identities`: a user's login, an app's slug, or an installation's account login) exists purely so the UI can attribute principals. Requ writes itireAuth`, the self-verifying app-JWT routes (token mint, repo installation), and the background refresher's installations listing — never by extra GitHub calls.

Dashboard routes (session-cookie auth, not bearer tokens):

- `GET /` — the dashboard page
- `GET /login` → `GET /auth/callback` — GitHub OAuth sign-in
- `POST /logout` — clear the session
- `GET /api/me` — `{ authenticated, login_configured, login, is_admin }`
- `GET /api/cache?scope=mine|all` — global truth totals plus the signed-in user's principal(s) (`mine`) or every known principal (`all`, admin only)
- `GET /api/requests` — recent data-API requests and their cache disposition (hit/miss/passthrough/write), per-disposition totals, and cumulative route-shape groups (`groups`: method + normalized route with per-disposition counts, sorted by total, since start) (admin only. In-memory, resets on start). The payload also carries the SQLite database's on-disk size (`db_size_bytes` plus `db_wal_size_bytes` for the `-wal` sidecar, statted per request from `DB_PATH`. Omitted when the file is missing), which the Requests tab renders on its summary line as e.g. `DB 1.4 GB (+125.8 MB WAL)`.
- `GET /api/webhooks` — recent webhook deliveries and their dispositions (admin only)
- `GET /api/timeline?since=<id>` — timed traffic events for the Timeline chart (JSON by default. The chart asks for a compact columnar binary encoding by exact media type): every webhook delivery try. Every served data-API request, every upstream fetch/probe/client exchange/login relay, and every notification try — each with its real measured duration, lane. Tooltip fields, plus `max_id` (the next `since` cursor) and `retention_start` (the 24h window floor). Omit `since` (or pass 0) for the full retained window (admin only. In-memory, resets on start)
- `GET /api/jobs?limit=<n>` — recent GitHub Actions jobs recorded from `workflow_job` webhooks: running jobs first (newest started first), then completed (newest completed first). `limit` defaults to 100, capped at 500 (admin only)
- `GET /api/ratelimit` — `{live, observed, note?}`: the GitHub App's per-installation `GET /rate_limit` poll (`live`) plus the passively observed `X-RateLimit-*` readings per (identity, resource) (`observed`. In-memory, resets on start). With no App configured (or a failed poll) `live` is empty and `note` explains why — the observed half is returned regardless (admin only)
- `GET /api/notifications` — subscriber-notification observability: cumulative delivery counters (`delivered`/`failed`/`gated`/`auto_disabled`), recent delivery attempts (in-memory ring, resets on start), and every subscription with its principal — secrets redacted (admin only)
- `GET /api/cache/data` — the global truth rows as flattened JSON. With `?principal=<key>` instead returns that principal's live (unexpired) access grants (admin only. `<key>` is the full principal key, e.g. `user:6569500` or `app:3433933`)
- `GET /api/cache/check[?org=<owner>]` — consistency-check diff of global truth against GitHub's live state (admin only. Requires a configured GitHub App. Strictly read-only)
- `POST /api/cache/check?apply=true[&org=<owner>]` — the same check, then RECONCILE: correct the drift from the fetched snapshot and report an `applied` tally (admin only. `?apply` on a GET is rejected with 405)
- Both check modes also take `?stream=1`, answering `application/x-ndjson`: one JSON progress line per phase (owners announced, repos fetched page by page, per-owner diff/apply done), flushed live, with the full report. This is as the last `{"phase":"report"}` line. This is what drives the dashboard modal's live progress bar.

Sign-in requires a GitHub OAuth App. Set `GITHUB_OAUTH_CLIENT_ID` / `GITHUB_OAUTH_CLIENT_SECRET` (see below). With those unset the page still renders but the sign-in button is disabled.

## Configuration

The service has **no static service token**. API requests authenticate with the caller's own bearer token. The only credential the service itself uses is an optional GitHub App, for background work. This is periodic refreshes and the consistency check's source-of-truth fetch. It also reads the Checks API for a caller whose token GitHub gives no Checks scope. Once that caller has proven it can read the repository.

| Variable | Required | Default | Description |
|---|---|---|---|
| `GITHUB_APP_ID` | No | — | GitHub App ID. When set (together with a private key), the service signs in as this app for background work — periodic refreshes and the consistency check — minting a short-lived access token per installation. API callers always authenticate with their own `Authorization` header. The app answers for one only on the Checks API, which GitHub closes to a fine-grained PAT, and only after the caller has proven read access to the repository. Leave unset to disable periodic refreshes and the consistency check; webhooks maintain global truth either way. |
| `GITHUB_APP_PRIVATE_KEY` | With `GITHUB_APP_ID` | — | The app's PEM private key (PKCS#1 or PKCS#8). May be `\n`-escaped onto a single line for convenience in env vars. |
| `GITHUB_APP_PRIVATE_KEY_PATH` | Alt. to inline | — | Path to a PEM private-key file. Takes precedence over `GITHUB_APP_PRIVATE_KEY`. |
| `WEBHOOK_SECRET` | For `/webhook` | — | HMAC secret for webhook signature verification. If unset, `POST /webhook` fails closed and rejects every delivery. |
| `LISTEN_ADDR` | No | `:8080` | HTTP listen address |
| `DB_PATH` | No | `github-mirror.db` | SQLite database file path |
| `SUBSCRIPTIONS_DB_PATH` | No | derived from `DB_PATH` | Subscriber-notification config DB — a **separate** SQLite file that survives the cache DB's schema nukes. Default strips a trailing `.db` from `DB_PATH` and appends `-subscriptions.db` (`github-mirror.db` → `github-mirror-subscriptions.db`). |
| `ALLOWED_ORIGINS` | No | `*` | Comma-separated CORS allow-list for browser clients. Safe to leave open because the cache reveals data per the caller's proven GitHub access, not origin. |
| `CACHE_MAX_ROWS` | No | `1000000` | Per-table row ceiling for the response caches (LRU rows beyond it are pruned on every write). One knob for every cache table: all but `git_commits_cache` are TTL-bounded (~24 h backstop or token expiry), so for them the cap is only a runaway safety net; `git_commits_cache` (immutable rows, no TTL) is the one table that actually grows to the ceiling. A value that is unparseable or < 1 fails startup. |
| `PASSTHROUGH_DEBOUNCE` | No | `5s` | How long an uncacheable (passthrough) read is held so identical concurrent requests share one upstream call. `0` forwards immediately. Deliberately makes uncacheable endpoints slow — the delay is the disincentive. Unparseable, negative, or > 30s fails startup. |
| `REFRESH_INTERVAL` | No | `6h` | Periodic fleet-refresh cadence (a Go duration string, e.g. `30m`). The first cycle always runs immediately at startup regardless of the interval. A value that is unparseable or not positive fails startup. |
| `WEBHOOK_REPLAY_INTERVAL` | No | `5m` | How often the service asks GitHub to re-send deliveries it could not hand over. GitHub never retries one by itself, so a lost delivery otherwise leaves stale answers being served for a full TTL. Needs a GitHub App. `0` disables recovery (the server warns). Unparseable or negative fails startup. |
| `GITHUB_OAUTH_CLIENT_ID` | For dashboard login | — | GitHub OAuth App client ID. Register the app's callback URL as `<BASE_URL>/auth/callback`. |
| `GITHUB_OAUTH_CLIENT_SECRET` | For dashboard login | — | GitHub OAuth App client secret. |
| `SESSION_SECRET` | Recommended | random per-process | HMAC key for signed session cookies. If unset, a random key is generated at startup, so sessions reset on restart. Set it in production. |
| `ADMIN_LOGINS` | No | `PazerOP` | Comma-separated GitHub logins granted the admin dashboard views — all principals, truth browse, consistency check (case-insensitive). |
| `BASE_URL` | No | derived from request | Public base URL (e.g. `https://github-state-mirror.pazer.io`) used to build the OAuth `redirect_uri`. Derived from the request (honoring `X-Forwarded-Proto`) when unset. |

## Building

```sh
npm ci && just build   # dashboard JS — the Go embed needs it on disk
go-toolchain
```

Binary is output to `build/server` (a GOOS=cosmo fat APE covering linux/amd64, darwin/arm64, and windows/amd64). The gosmopolitan fork aliases GOOS=cosmo to `linux` for build-tag matching, so third-party modules with no dedicated cosmo port build unmodified. There is no per-repo step.

Start it through a shell (`sh build/server`): the kernel cannot exec an APE directly. The shell runs the boot script, which stages a runnable copy under `/tmp`. The container image does the same thing, which is why it carries busybox.

`just test-image` builds the image, starts it, and requires it to serve. It needs docker and the binary above, and CI gates the GHCR publish on it.

The dashboard front-end's only source is TypeScript, under `internal/api/web/src/`. `just build` compiles it to `internal/api/web/assets/*.js` — a build output, gitignored and never committed — which `//go:embed` then bakes into the binary. Build the JS before building the server:

```sh
npm ci        # first time only
just build # tsc: src/*.ts -> assets/*.js (required before go-toolchain)
```

CI builds the JS itself in every job that needs it, and `web-check` fails if any generated `.js` is ever committed. A `preview` job deploys a standalone, backend-free styling preview of the dashboard to buildhost for each branch, served at `https://sites.pazer.build/github-state-mirror/branch/<branch>/`.

Each `src/*.ts` file emits its own standalone ES module loaded by its own `<script type="module">` tag (`rate-meter.ts` self-registers the `<rate-meter>` web component behind. The rate-limit tiles. `timeline.ts` self-registers the `<gsm-timeline>` element behind the admin Timeline tab). A new asset file must also be added to the `//go:embed` + hashed-URL wiring in `internal/api/dashboard.go` and the `preview` job's copied-assets list in `.github/workflows/ci.yml`.

The Timeline chart's `<timeline-view>` component is **not vendored**: the browser imports it at runtime from `https://sites.pazer.build/js-snippets/branch/library/ui/timeline-view.js` (live at master head. This is the org's standard js-snippets consumption model. Replaced the quota-dead GitHub Pages deploy), so component fixes reach this dashboard on js-snippets merge with no mirror change. Fix component bugs upstream in js-snippets. A failed component fetch degrades softly: the tab shows "chart loading…". The tab retries the import on a fixed 5s cadence forever, while every other tab is unaffected.

Types for the URL import come from `src/js-snippets-timeline.d.ts`, an interim hand-maintained ambient shim.

## Docker

A container image is published to the GitHub Container Registry on every push to `master`:

```
ghcr.io/wow-look-at-my/github-state-mirror:latest
```

It is a static binary on a `distroless` base plus busybox, which supplies the shell the APE needs to start. It runs as a non-root user, listens on port `8080`, and keeps its SQLite cache under `/var/lib/github-state-mirror` (declared as a volume). As a result, `DB_PATH` defaults to `/var/lib/github-state-mirror/github-mirror.db` inside the container.

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

The SQLite database is a disposable cache, so persisting it with a volume is optional. The `publish-ghcr` job in `.git builds the imagehub/workflows/ci.yml`, which reuses `wow-look-at-my/actions/.github/workflows/publish-ghcr.yml` (restores the CI build output handed off via the actions cache, builds the `Dockerfile`, pushes to GHCR, and prunes old versions).

## Architecture

```
internal/
  actor/         Context-based principal-key propagation (who is asking)
  freshness/     Generic cache freshness framework (no GitHub knowledge)
  database/      SQLite schema + sqlc-generated queries
  ghdata/        Domain store (wraps sqlc with transactions)
  ghclient/      GitHub REST + GraphQL client (token→login cache)
  ratemeter/     In-memory passive X-RateLimit-* observation (per identity+resource)
  auth/          GitHub OAuth login + signed-cookie sessions (dashboard)
  sync/          Fetchers, periodic refresh, webhook dispatch
  webhook/       HTTP handler, event parsing, HMAC verification
  api/           chi router, REST handlers, GraphQL assembler, web dashboard
  api/web/       Dashboard front-end (TypeScript src/ -> built, embedded assets/)
```

The key design constraints: `internal/freshness/` is a generic cache-coherence engine that knows nothing about GitHub. The `internal/sync/` package bridges the freshness framework with GitHub-specific data. Cached data is ONE global truth store. Permission** above) proven the show layer prevents Cross-user leakage, which serves a repo's data only to principals whose own GitHub access has.
