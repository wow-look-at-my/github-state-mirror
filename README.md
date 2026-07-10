# github-state-mirror

A Go service that mirrors GitHub state into a local SQLite database, providing fast API access with automatic cache management.

## How It Works

Data stays fresh via three mechanisms:

1. **Webhooks (primary)** — GitHub pushes events and we apply the payload straight to the cache, with **no re-fetch**:
   - `pull_request` / `pull_request_review` → upsert the PR (and delete it on close)
   - `status` / `check_run` / `check_suite` → record the per-check state and recompute the commit-status rollup onto any PR whose head is that commit, and onto the repo's default-branch status when the check ran on the default branch
   - `push` → update the repo's `pushed_at`
   - `label` → recolor/remove the label across the repo's cached PRs

   - `repository` → lifecycle: a deleted repo is removed (cascade), a renamed one moves, privatized/publicized updates the stored visibility

   Only a structural payload that can't be parsed falls back to marking the affected resource stale for lazy refresh.

   **Webhooks always apply.** There is ONE global truth store, so a delivery never asks "who has this cached?" — an event for a repo nobody has ever fetched upserts the repo row straight from the payload's own `repository` object and applies. There is no "skipped" outcome and no on-demand fetch on the webhook path.
2. **Periodic fleet refresh** — When a GitHub App is configured (see Configuration), every 6 hours the service signs in as the app — per installation, never using a static service token — and syncs **each installation's account**: every repo + open PR of that owner, Organizations and Users alike (an owner-agnostic `repositoryOwner` GraphQL query). Only repos the account actually **owns** are synced (`ownerAffiliations: OWNER`, plus a client-side guard that drops — and logs — any foreign-owner node): a User's *collaborator* repos belong to their real owners and are never keyed under the user. Each sync lands in global truth and earns the installation's stable `app-installation:<id>` principal its grants; a brand-new installation is synced on the first cycle (the refresher names the owner itself — it does not wait for a pre-existing freshness marker).
3. **Lazy fetch** — On first access (or cache miss), data is fetched on demand — with the caller's own token — before responding

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

### One global truth, revealed by permission

There is **one cache because there is one true state** (operator directive, 2026-07-03): webhooks and every caller's fetches all maintain the same global rows, and what keeps mutually-untrusting callers apart is the **reveal layer** — the appropriate parts of the cache are revealed to a caller based on whether they could access the resource directly through GitHub.

- **Principals.** Every request's bearer token resolves to a principal: `user:<numeric user id>` for user tokens (`GET /user`, cached per token; stable across renames, shared by all of a user's tokens), a SHA-256 token fingerprint for tokens that are definitively not users, or `app:<id>` when the caller asserts a verified App JWT (below). The principal decides what is *revealed*, never what is *stored*.
- **Revealing a repo requires proof of GitHub access.** A cached resource under `owner/repo` is served iff: the repo is **public** in truth (fast path, no GitHub call); the principal holds a live **access grant**; or a live **probe** — `GET /repos/{owner}/{repo}` with the caller's own token — answers 2xx. Grants are only ever earned from GitHub's own answers: an org list-sync run with the caller's token replace-syncs their grants to exactly the repos GitHub returned (`list_sync`), and a successful probe records one (`probe`). Grants expire (~24h), so revoked GitHub access converges; expired grant/deny rows are swept opportunistically from the grant-writing paths (`SyncOrgTruth`, `RecordGrant`), throttled to at most once per ~10 minutes — reads always filter on expiry regardless.
- **Denials are relayed, and only authoritative ones are cached.** A probe answering 404 (or a non-rate-limit 403) is relayed to the caller and cached (~5m) so repeat probing doesn't hammer GitHub. Transient failures (5xx, 429, rate-limited 403, network) are never cached — they fail only that one request.
- **Fail closed.** A repo whose visibility is unknown is treated as private. A transient `/user` failure with no cached identity verdict fails 503 rather than guessing a principal.
- **The one-truth consequence:** callers do not each pay to warm "their" copy — a webhook or any principal's fetch keeps the single copy current for everyone allowed to see it. One user still can never read another's private-repo data: revealing requires GitHub to have answered *that* caller.

#### App-identity principal (for trusted first-party app callers)

A **GitHub App backend** whose installation tokens rotate hourly would earn grants on a fresh fingerprint principal every hour. Such a caller may instead assert a stable identity by sending its **GitHub App JWT** in an `X-Mirror-Identity` header. The mirror verifies that JWT against GitHub (`GET /app` — unforgeable, since only the app's private key produces a JWT GitHub accepts) and resolves the caller to the principal `app:<id>`, so all of the app's rotating tokens share **one** stable grant set. The `Authorization` token is still used for upstream fetches and probes, so per-repo authorization against GitHub is preserved. Callers that send no identity header keep the default resolution above unchanged — this mode is opt-in and additive.

### REST (cached routes — state-absorbed, rebuilt trimmed)

Ten REST routes are served from cache (repo-scoped ones behind the reveal layer above). They do **not** replay GitHub's bytes:
the mirror **absorbs the state** contained in the response into structured
tables and **rebuilds a trimmed response** from that state, with **every URL
field dropped** — `url`, anything matching `*_url` (`html_url`, `git_url`,
`download_url`, `documentation_url`, ...), and `_links`. Consumers are
first-party tooling (pr-minder etc.) that read state fields only. Hits and
misses both serve the rebuilt shape, marked with an `X-GSM-Cache: hit|miss`
header; requests the route cannot model (a non-default `Accept` such as
`application/vnd.github.raw`, unknown query params, an unexpected body shape)
pass through verbatim, uncached.

- `GET /repos/{owner}/{repo}/contents/{path}` (incl. `?ref=`) — a `200` file
  (`{type, encoding, size, name, path, content, sha}`), a `200` directory
  listing (entries as `{type, size, name, path, sha}`), **and a `404`**
  (rebuilt as `{"message": ..., "status": "404"}`) are all cached — the 404
  "config file absent" answer is half the win for config probes. Invalidated
  by `push` and `repository` webhooks (conservative whole-repo flush) with a
  24 h TTL backstop so a missed webhook can never serve stale state forever.
- `GET /repos/{owner}/{repo}/git/commits/{sha}` — rebuilt as
  `{sha, author, committer, message, tree: {sha}, parents: [{sha}, ...]}`.
  Immutable content: no invalidation, no TTL, bounded only by LRU pruning.
  **Push webhooks also feed this cache**: the payload's commits (id, tree,
  message, timestamp, author/committer, with parents derived from the
  payload's linear chain) are absorbed on delivery, so the common post-push
  read hits without any GitHub fetch ever having happened.
- `GET /repos/{owner}/{repo}/commits` — the commits list (pr-minder's
  fork-point detection pages this per branch). Each listed commit is absorbed
  into the same `git_commits_cache` rows as above; a per-query snapshot —
  keyed by the raw `sha` query value (`''` = default branch), `per_page`, and
  `page` — stores only the response's ordered shas, so the list rebuilds from
  the immutable commit rows in the exact order GitHub returned. Item shape:
  `{sha, commit: {author, committer, message, tree: {sha}}, parents: [{sha},
  ...]}`. Only `sha`/`per_page` (1..100)/`page` (1..10) are modeled; anything
  else (`path`, `since`, `author`, ...) passes through, and only a `200` is
  absorbed (404/409/5xx relay unstored). Snapshots are flushed by `push` and
  `repository` webhooks (a push moves every ref-relative listing; the
  absorbed commit rows stay) with a 24 h TTL backstop. The single-commit
  sub-path `/commits/{sha}` is a different shape and stays passthrough.
- `GET /repos/{owner}/{repo}/compare/{basehead}` — the three-dot
  `base...head` comparison (pr-minder's empty-PR / open-PR gates run one per
  branch, every fleet sweep). Rebuilt as `{status, ahead_by, behind_by,
  total_commits, merge_base_commit: {sha}, commits: [<the commits-list item
  shape>], files: [{filename, status, additions, deletions, changes,
  previous_filename?}]}` — the **files array's presence and length are
  preserved exactly** (consumers read `changed_files = files.length`; an
  absent array means "unknown, fail open", an empty one means a 0-diff
  branch), while URL fields and the unread per-file `patch` are dropped. The
  whole trimmed document is cached per exact basehead (branch names keep
  their slashes); the compare's commits are also absorbed into
  `git_commits_cache`. Only the bare query shape with the default JSON
  `Accept` is modeled — any query param, the `.diff`/`.patch` media types, a
  cross-fork `owner:branch` basehead, or a basehead without `...` passes
  through — and only a `200` is absorbed (404/5xx relay unstored). Flushed by
  `push` and `repository` webhooks (either side of any comparison may have
  moved) with a 24 h TTL backstop.
- `GET /repos/{owner}/{repo}/commits/{ref}/status` and
  `GET /repos/{owner}/{repo}/commits/{ref}/check-runs` — the combined commit
  status and the check-runs listing for a ref (what fleet-wide CI watchers
  poll per repo/branch/sha). The `{ref}` is cached **verbatim** — a branch
  name (slashes and all), a sha, or a tag, never resolved — so each spelling
  is its own snapshot. Rebuilt as `{state, sha, total_count, statuses:
  [{context, state, description, created_at, updated_at}]}` and
  `{total_count, check_runs: [{id, head_sha, name, status, conclusion,
  started_at, completed_at, app: {id}}]}` — nullable fields (a status's
  description; an in-progress run's conclusion/completed_at) are preserved
  as null, while URL fields (including per-status `target_url` and per-run
  `html_url`/`details_url` — no known consumer reads them through the
  mirror), the repository object, and the unbounded check-run `output` are
  dropped. Only the bare query shape with the default JSON `Accept` is
  modeled (any query param passes through), and only a `200` is absorbed
  (404/5xx relay unstored). Flushed by `status`/`check_run`/`check_suite`
  events (CI state moved somewhere in the repo) plus `push` and `repository`
  webhooks, with a 24 h TTL backstop — so snapshots survive exactly while a
  repo's CI is quiet, which is when watchers re-poll. Other `/commits/*`
  sub-paths (the single-commit read, `/statuses`, `/check-suites`, ...) stay
  passthrough.
- `POST /app/installations/{id}/access_tokens` — the installation-token mint.
  The bearer here is a GitHub App JWT; the mirror verifies it (`GET /app`) and
  caches the minted `201` per (app, installation, request-body hash), serving
  `{token, expires_at, permissions, repository_selection}` until 10 minutes
  before the token's real expiry. Invalidated by `installation` /
  `installation_repositories` events. Unverifiable callers pass through.
- `GET /repos/{owner}/{repo}/pulls` — the open-PR list, rebuilt from the same
  webhook-maintained global PR state the GraphQL cache uses. Serving a list
  needs a completeness proof, so it is gated by a per-repo "open-PR list
  complete" marker: set when a full unfiltered page-1 response is absorbed,
  maintained by `pull_request` webhooks (which update the rows, never the
  marker), and bounded by a 24 h TTL. Only the query shapes its consumers send
  are modeled (`state=open`, `per_page`, `page=1`, `head=owner:branch`);
  anything else — and any rebuilt list as long as the requested `per_page`
  (more pages may exist upstream) — passes through/misses.
- `GET /repos/{owner}/{repo}/pulls/{number}` — a single open PR, served from
  state only when the cached row carries the REST fields, was **recently
  touched** (a 24 h staleness backstop, so a missed `closed` delivery cannot
  serve a stale row forever), AND has a **known** `mergeable`. GitHub computes
  `mergeable` lazily and pr-minder polls this endpoint waiting for it to
  resolve, so a null/unknown mergeable always misses (the fetch absorbs
  GitHub's computed answer), and a push to a PR's base or head branch
  un-resolves the stored value — the cache can never wedge that poll on a
  stale answer. Closed PRs are never stored (absorbing one deletes the cached
  row); `Accept: application/vnd.github.diff` (the pr-minder diff read)
  passes through.
- `GET /repos/{owner}/{repo}/installation` — the App-level "which installation
  covers this repo" lookup. App-JWT verified like the token mint, cached per
  app as `{id, account{login,type}, repository_selection, app_id, app_slug,
  target_type}`, flushed by `installation`/`installation_repositories` events
  plus a 24 h TTL.

Cached state is global truth, revealed per caller by the reveal layer; caps +
LRU pruning bound each response-cache table. (Only the App-credential caches —
minted tokens and repo-installation lookups — stay keyed by the app principal:
a minted token is per-credential, not shared truth.)

(The byte-identity rule now applies only to the GraphQL org-repos route below;
the trimmed rebuild contract above is the operator-chosen model for cached REST
routes.)

### GraphQL

- `POST /graphql` — the org-repos query (an `organization { repositories { ... pullRequests ... } }` shape) is served from the cache, assembled from SQLite **to match exactly the fields GitHub returns for that query's selection set** (validated by an identity test against a recorded GitHub response). Any other GraphQL query is forwarded to GitHub (see passthrough below).

### GitHub passthrough (everything else)

Any request the mirror does not serve from cache is **transparently forwarded to GitHub** (`https://api.github.com`) and returned **uncached**. This makes the mirror a drop-in replacement for `api.github.com`: the endpoints above are served fast from the global cache, and every other endpoint (`/rate_limit`, `/repos/{owner}/{repo}`, issues, releases, an unrecognized GraphQL query, ...) still works. A 2xx passthrough under `/repos/{owner}/{repo}/...` also refreshes the caller's grant — GitHub just proved their access. Mutating methods are recorded as `write` in the request log (they are proxied because they mutate, not because caching failed).

- The caller's `Authorization` header is forwarded unchanged — the mirror never substitutes its own `GITHUB_TOKEN` — and a forwarded request **still requires a token** (`401` otherwise), so the mirror is never an open, unauthenticated relay.
- Responses are passed through verbatim, including status, body, and headers such as `Link` (pagination) and `X-RateLimit-*`. The mirror's own CORS headers are authoritative; GitHub's duplicate `Access-Control-Allow-*` are stripped while `Access-Control-Expose-Headers` is preserved so browsers can read those rate-limit/link headers.
- This path is uncached: it never reads or writes the freshness store.

### OAuth token-exchange relay

- `POST /login/oauth/access_token` — relays a GitHub OAuth "exchange code for token" request to `github.com` and returns the response with the mirror's CORS headers. A purely client-side app (e.g. the repo-nightmare PR viewer) cannot call GitHub's token endpoint directly because it sends no CORS headers; the mirror stands in as the CORS-correct relay. It carries **no** bearer token (the OAuth `client_secret` in the body is the credential), so it is unauthenticated, and it targets `github.com` — not the `api.github.com` passthrough.

### Webhook

- `POST /webhook` — receives GitHub webhook events and applies them to the cache. The handler processes each delivery **synchronously** (the cache writes are small, idempotent upserts that finish well within GitHub's delivery deadline) and the HTTP response reflects what happened, so a "successful" delivery actually means data was preserved:
  - `200 OK` — applied: webhook data was written to global truth
  - `202 Accepted` — received but nothing applied (an untracked event/action, or a fallback invalidation)
  - `500` — an internal error; GitHub retries (and the re-applied upsert is idempotent)

  The disposition and detail are returned in the JSON body and an `X-GSM-Disposition` header, and every delivery is recorded in the dashboard's webhook log (see below).

  Besides the PR/check/push/label/repository events that feed global truth, the mirror also tracks **`workflow_job`** deliveries (`in_progress` and `completed`; `queued`/`waiting` are dropped as `ignored`), recording GitHub Actions job state as it happens in a **global** table read via the admin-only `GET /api/jobs`. The GitHub App must be **subscribed to the `workflow_job` event** in its settings to receive these; expect high volume in a CI-heavy org — each delivery costs one cheap synchronous SQLite upsert.

### Subscriber notifications (`/_mirror/subscriptions`)

Consumers that receive their own GitHub webhooks and immediately query the mirror **race its ingestion**: their delivery can arrive before the mirror's, and the read returns pre-event state. Subscriber notifications remove the race — the mirror POSTs a compact, HMAC-signed JSON notification to your endpoint **after** its synchronous dispatcher has applied the delivery to global truth, so keying your reads off the mirror's notification always reads post-ingest state.

The top-level **`/_mirror/*` prefix is the reserved mirror-native namespace**: GitHub's API has no underscore-prefixed top-level paths, and registered routes always win over the passthrough proxy, so nothing under `/_mirror/*` can ever collide with proxied GitHub traffic.

**Registering** (same bearer-token auth as every data route; subscriptions are owned by the principal your token resolves to, so an app using `X-Mirror-Identity` keeps its subscriptions across token rotations):

```sh
# Create (201). repos/events are optional filters; empty = everything you may see.
curl -X POST https://mirror.example.com/_mirror/subscriptions \
  -H "Authorization: Bearer $GITHUB_TOKEN" \
  -d '{"url":"https://consumer.example.com/mirror-hook","secret":"<16..256 chars>","repos":["my-org","my-org/one-repo"],"events":["push","pull_request"]}'

curl -H "Authorization: Bearer $GITHUB_TOKEN" https://mirror.example.com/_mirror/subscriptions          # list your own
curl -H "Authorization: Bearer $GITHUB_TOKEN" https://mirror.example.com/_mirror/subscriptions/sub_...  # one (404 for a foreign id)
curl -X PATCH -H "Authorization: Bearer $GITHUB_TOKEN" https://mirror.example.com/_mirror/subscriptions/sub_... \
  -d '{"active":true}'                                                                                  # partial update / re-enable
curl -X DELETE -H "Authorization: Bearer $GITHUB_TOKEN" https://mirror.example.com/_mirror/subscriptions/sub_...  # 204
```

Subscription JSON: `{id, url, repos, events, active, consecutive_failures, disabled_reason, created_at, updated_at, last_success_at, last_failure_at, last_error}`. The `secret` is **never** returned by any response. `url` must be absolute https with no userinfo (plain http is allowed only for loopback hosts, so local receivers work) and must not be a private/link-local IP literal; `repos` entries are `owner` or `owner/repo` (matched case-insensitively); `events` are GitHub event names (`push`, `pull_request`, ...). Each principal may hold at most 20 subscriptions (409 beyond).

**Deliveries** are `POST`s with `Content-Type: application/json`, `X-Mirror-Event` (event name), `X-Mirror-Delivery` (the notification id), and `X-Hub-Signature-256: sha256=<hex HMAC-SHA256(secret, raw body)>` — **GitHub's exact signature scheme**, so the webhook verification code you already have works unchanged. Example payload (fields absent from the event are omitted):

```json
{
  "mirror_delivery": "ntf_9f0c...",
  "subscription_id": "sub_5a1b...",
  "github_delivery": "b6bf1c50-...",
  "event": "pull_request",
  "action": "opened",
  "owner": "my-org",
  "repo": "my-repo",
  "repo_full_name": "my-org/my-repo",
  "pr_number": 123,
  "sha": "0a1b2c...",
  "disposition": "applied",
  "ingested_at": "2026-07-10T12:00:00.123456789Z",
  "sent_at": "2026-07-10T12:00:00.234567890Z"
}
```

PR events carry `pr_number` + the head `sha`; pushes carry `ref` + the after `sha`; status/check/workflow-job events carry the commit `sha`. **Correlation note:** `github_delivery` is the `X-GitHub-Delivery` GUID **the mirror received** — it is NOT the GUID GitHub sent *you* for the same event (GitHub mints one per receiver). Correlate on `owner`/`repo` plus the identifier fields instead.

Delivery semantics:

- Only dispositions **`applied`** and **`invalidated`** notify (`ignored`/`error` don't), and only deliveries that resolve to a single `owner/repo` — repo-less events (`installation`, `installation_repositories`) never notify.
- Any **2xx** from your endpoint counts as delivered. A failed delivery is retried up to 3 attempts (short backoff, ~8s per attempt); a terminal failure increments `consecutive_failures` and stamps `last_failure_at`/`last_error`.
- At **10 consecutive terminal failures** the subscription is **auto-disabled** (`active=false`, `disabled_reason` set). Re-enable it after fixing your endpoint with `PATCH {"active": true}`, which also resets the failure counter.
- Notifications are fanned out **after** the webhook response to GitHub — subscriber endpoints can never slow the mirror's ingestion.

**Authorization (reveal-gated, fail closed):** a subscription is notified about a repo only if its principal could read that repo through the reveal layer's non-probing fast paths at delivery time — the repo is **public** in global truth, or the principal holds a **live grant**. There is no per-notification probe (no caller token exists at delivery time); unknown visibility reads as private, so a private repo's activity never reaches a principal that has not proven access. Earn the grant the normal way (an org list-sync or any revealed read with your token) before expecting private-repo notifications.

**Persistence:** subscriptions are service **config**, not cache. They live in their own SQLite file (`SUBSCRIPTIONS_DB_PATH`, default derived from `DB_PATH`: `github-mirror.db` → `github-mirror-subscriptions.db`) which **survives the cache DB's SchemaVersion nukes** and every deploy.

**Operator view:** admin-only `GET /api/notifications` (dashboard session auth) returns in-memory delivery counters (`delivered` / `failed` / `gated` / `auto_disabled`), the recent delivery attempts (bounded ring; resets on restart), and every subscription with its principal (secrets redacted). JSON only — there is no dashboard tab.

## Web Dashboard

Visit the service root (e.g. `https://github-state-mirror.pazer.io/`) and **sign in with GitHub** to see the state of the cache: the global truth totals (repos, pull requests, commit checks, contents, git commits, grants — one cache, one truth), your principal's reveal-layer standing (how many repos you hold live grants for), org-sync freshness, and recent refresh activity.

- **Truth is shared; grants are yours.** The totals are the one global store's counts — the same numbers for every viewer. What is per-you is the reveal layer: your principal's live grant count and sync freshness. The dashboard never exposes cached rows to non-admins.
- **Admins see everything.** Logins listed in `ADMIN_LOGINS` (default `PazerOP`) get a **Principals** view: every known principal with its login, live grant count, and last-seen time, plus a per-principal **Grants** action showing exactly which repos the reveal layer will serve it (and whether each grant came from a `list_sync` or a `probe`). They also get two global activity logs: a **Requests** tab — live data-API traffic and how each request was served (`hit` from cache / `miss` then fetched / `passthrough` read forwarded uncached / `write` mutation proxied), plus two grouped tables above the flat history — **Top requests** and **Top uncached requests** — aggregating traffic by route shape (e.g. `/repos/{owner}/{repo}/compare/{basehead}`) since restart, so the hottest routes and the hottest uncached ones (the caching candidates) are visible at a glance (in-memory like the rest of the request log) — and a **Webhooks** tab — recent webhook deliveries and each one's disposition (`applied` / `invalidated` / `ignored` / `error`; there is no "skipped" under the global model), confirming that incoming events update truth. A **Rate limit** tab shows GitHub rate-limit standing two ways: **live** — a `GET /rate_limit` poll per installation of the mirror's own GitHub App (the credential behind background refreshes and the consistency check) — and **observed** — the `X-RateLimit-*` headers GitHub attaches to every response, passively recorded off each upstream path (passthrough proxy, cache-miss fetches, reveal probes, the client's own calls) and grouped per identity (principal or credential-derived label; never a raw token) and resource bucket. The observed half costs zero API calls and covers the callers' own credentials, not just the App; zero-usage readings (nothing consumed in the current window, e.g. only 304s) are hidden as noise; it is in-memory like the request log and resets on restart. Without a GitHub App the tab still works, showing observed data plus a note that live polling is unavailable.
- **Admins can browse and audit global truth.** The Principals tab carries two truth-wide actions:
  - **Browse truth** opens the actual cached rows — repositories (with stored visibility), open pull requests with their labels and CI status, and commit checks — plus a copyable raw-JSON dump.
  - **Run consistency check** re-fetches the source of truth from GitHub (as the mirror's own GitHub App, via the owner-agnostic `repositoryOwner` query — User-account installations are checked like Organizations) and emits a JSON diff of any drift between GLOBAL truth and GitHub — repos/PRs only in the cache or only on GitHub, and field mismatches such as a stale `last_commit_status`, `default_branch_status`, `visibility`, `is_archived`, `pushed_at` (5-minute tolerance; lag implies missed push webhooks), `auto_merge_method`, draft flag, or labels (`mergeable` is deliberately not compared: the cache un-resolves it on pushes). A repo cached **public** that GitHub says is private/internal gets its own `visibility_leak` issue (the reveal fast path is serving it); a cached-only repo that is **archived** is classified as expected (own tally) rather than drift; cached-open PRs under a missing repo are swept as their own entries; PR-existence entries carry `served_now` (a live pulls-list marker means the wrong list is being served right now) and every discrepancy carries a short `fix` hint. A missing repo that is **private** on GitHub is marked and tallied separately (`repos_only_on_github_private`) — under lazy truth it simply has not been absorbed yet, which is expected, not a failure. The report includes `truth_freshness`: per owner, the most recent org list-sync any principal ran. It needs a configured GitHub App; without one the action reports "unavailable". Owners the app is not installed on are listed as skipped rather than reported as missing.
  - **Reconcile** runs the same check and then **corrects** the drift from the same fetched snapshot (`POST /api/cache/check?apply=true`, behind a confirmation): missing repos/open PRs are absorbed into truth (grants recorded under the installation principal), stale cached-open PRs are deleted, `visibility` / `default_branch_status` / `auto_merge_method` are set from GitHub's answers **including nulls** the COALESCE upserts can never write, and a poisoned CI rollup (a ghost PENDING `commit_checks` row whose completion delivery was missed) is fixed by deleting the contradicted rows and setting GitHub's verdict — a correction that survives the next PR webhook. The response is the normal report plus an `applied` tally per action. A plain GET stays strictly read-only. Both actions stream live NDJSON progress (`?stream=1`) so the modal shows a per-owner, per-page progress bar instead of sitting silent for the minutes a fleet run takes.
- **Separate from the data API.** Dashboard authorization is by GitHub login (an OAuth session cookie), distinct from the data API's bearer-token + reveal model. The principal→login mapping (`actor_identities`) exists purely so the UI can attribute principals.

Dashboard routes (session-cookie auth, not bearer tokens):

- `GET /` — the dashboard page
- `GET /login` → `GET /auth/callback` — GitHub OAuth sign-in
- `POST /logout` — clear the session
- `GET /api/me` — `{ authenticated, login_configured, login, is_admin }`
- `GET /api/cache?scope=mine|all` — global truth totals plus the signed-in user's principal(s) (`mine`) or every known principal (`all`, admin only)
- `GET /api/requests` — recent data-API requests and their cache disposition (hit/miss/passthrough/write), per-disposition totals, and cumulative route-shape groups (`groups`: method + normalized route with per-disposition counts, sorted by total, since restart) (admin only; in-memory, resets on restart)
- `GET /api/webhooks` — recent webhook deliveries and their dispositions (admin only)
- `GET /api/jobs?limit=<n>` — recent GitHub Actions jobs recorded from `workflow_job` webhooks: running jobs first (newest started first), then completed (newest completed first); `limit` defaults to 100, capped at 500 (admin only)
- `GET /api/ratelimit` — `{live, observed, note?}`: the GitHub App's per-installation `GET /rate_limit` poll (`live`) plus the passively observed `X-RateLimit-*` readings per (identity, resource) (`observed`; in-memory, resets on restart). With no App configured (or a failed poll) `live` is empty and `note` explains why — the observed half is returned regardless (admin only)
- `GET /api/notifications` — subscriber-notification observability: cumulative delivery counters (`delivered`/`failed`/`gated`/`auto_disabled`), recent delivery attempts (in-memory ring, resets on restart), and every subscription with its principal — secrets redacted (admin only)
- `GET /api/cache/data` — the global truth rows as flattened JSON; with `?principal=<key>` instead returns that principal's live (unexpired) access grants (admin only; `<key>` is the full principal key, e.g. `user:6569500` or `app:3433933`)
- `GET /api/cache/check[?org=<owner>]` — consistency-check diff of global truth against GitHub's live state (admin only; requires a configured GitHub App; strictly read-only)
- `POST /api/cache/check?apply=true[&org=<owner>]` — the same check, then RECONCILE: correct the drift from the fetched snapshot and report an `applied` tally (admin only; `?apply` on a GET is rejected with 405)
- Both check modes also take `?stream=1`, answering `application/x-ndjson`: one JSON progress line per phase (owners announced, repos fetched page by page, per-owner diff/apply done), flushed live, with the full report as the final `{"phase":"report"}` line — this is what drives the dashboard modal's live progress bar.

Sign-in requires a GitHub OAuth App; set `GITHUB_OAUTH_CLIENT_ID` / `GITHUB_OAUTH_CLIENT_SECRET` (see below). With those unset the page still renders but the sign-in button is disabled.

## Configuration

The service has **no static service token**. API requests authenticate with the caller's own bearer token; the only credential the service itself uses is an optional GitHub App, exclusively for background work — periodic refreshes and the consistency check's source-of-truth fetch.

| Variable | Required | Default | Description |
|---|---|---|---|
| `GITHUB_APP_ID` | No | — | GitHub App ID. When set (together with a private key), the service signs in as this app for background work — periodic refreshes and the consistency check — minting a short-lived access token per installation. It is **never** used to serve API callers, who always authenticate with their own `Authorization` header. Leave unset to disable periodic refreshes and the consistency check; webhooks maintain global truth either way. |
| `GITHUB_APP_PRIVATE_KEY` | With `GITHUB_APP_ID` | — | The app's PEM private key (PKCS#1 or PKCS#8). May be `\n`-escaped onto a single line for convenience in env vars. |
| `GITHUB_APP_PRIVATE_KEY_PATH` | Alt. to inline | — | Path to a PEM private-key file. Takes precedence over `GITHUB_APP_PRIVATE_KEY`. |
| `WEBHOOK_SECRET` | For `/webhook` | — | HMAC secret for webhook signature verification. If unset, `POST /webhook` fails closed and rejects every delivery. |
| `LISTEN_ADDR` | No | `:8080` | HTTP listen address |
| `DB_PATH` | No | `github-mirror.db` | SQLite database file path |
| `SUBSCRIPTIONS_DB_PATH` | No | derived from `DB_PATH` | Subscriber-notification config DB — a **separate** SQLite file that survives the cache DB's SchemaVersion nukes. Default strips a trailing `.db` from `DB_PATH` and appends `-subscriptions.db` (`github-mirror.db` → `github-mirror-subscriptions.db`). |
| `ALLOWED_ORIGINS` | No | `*` | Comma-separated CORS allow-list for browser clients. Safe to leave open because the cache reveals data per the caller's proven GitHub access, not origin. |
| `GITHUB_OAUTH_CLIENT_ID` | For dashboard login | — | GitHub OAuth App client ID. Register the app's callback URL as `<BASE_URL>/auth/callback`. |
| `GITHUB_OAUTH_CLIENT_SECRET` | For dashboard login | — | GitHub OAuth App client secret. |
| `SESSION_SECRET` | Recommended | random per-process | HMAC key for signed session cookies. If unset, a random key is generated at startup, so sessions reset on restart. Set it in production. |
| `ADMIN_LOGINS` | No | `PazerOP` | Comma-separated GitHub logins granted the admin dashboard views — all principals, truth browse, consistency check (case-insensitive). |
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

Each `src/*.ts` file emits its own standalone ES module loaded by its own `<script type="module">` tag (`rate-meter.ts` self-registers the `<rate-meter>` web component behind the rate-limit tiles). A new asset file must also be added to the `//go:embed` + hashed-URL wiring in `internal/api/dashboard.go` and the `preview` job's copied-assets list in `.github/workflows/ci.yml`.

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
  api/web/       Dashboard front-end (TypeScript src/ -> embedded assets/)
```

The key design constraints: `internal/freshness/` is a generic cache-coherence engine that knows nothing about GitHub. The `internal/sync/` package bridges the freshness framework with GitHub-specific data. Cached data is ONE global truth store; cross-user leakage is prevented by the reveal layer, which serves a repo's data only to principals whose own GitHub access has been proven (public repo, live grant, or live probe — see **One global truth, revealed by permission** above).
