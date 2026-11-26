# API Endpoints

## Authentication

Every data endpoint **requires** a GitHub token — a personal access token or a GitHub App user-to-server token both work. Send it as a bearer token:

```
Authorization: Bearer <token>
```

Requests with no token are rejected with `401 Unauthorized` — they are never served using the service's own credentials (the GitHub App used for background refreshes). The token is validated against GitHub (`GET /user`) on first use and the result is cached in memory.

The only endpoint that does **not** require a bearer token is `POST /webhook`, which is authenticated by its GitHub HMAC signature instead (see `WEBHOOK_SECRET`).

## One global truth, revealed by permission

There is **one cache because there is one true state** (operator directive). Webhooks and every caller's fetches all maintain the same global rows, and what keeps mutually-untrusting callers apart is the **show layer**. The appropriate parts of the cache are revealed to a caller based on whether they can access the resource directly through GitHub.

- **Principals.** Every request's bearer token resolves to a principal: `user:<numeric user id>` for user tokens (`GET /user`, cached per token. Stable across renames, shared by all of a user's tokens), a SHA-256 token fingerprint for tokens that are definitively not users, or `app:<id>`. This is when the caller asserts a verified App JWT (below). The principal decides what is *revealed*, not what is *stored*.
- **Revealing a repo requires proof of GitHub access.** A cached resource under `owner/repo` is served iff: the repo is **public** in truth (fast path, no GitHub call). The principal holds a live **access grant**. Or a live **probe** — `GET /repos/{owner}/{repo}` with the caller's own token — answers 2xx. Grants are only ever earned from GitHub's own answers. An org list-sync run with the caller's token replace-syncs their grants to exactly the repos GitHub returned (`list_sync`). A successful probe records one (`probe`). Grants expire (~24h), so revoked GitHub access converges. Expired grant/deny rows are swept opportunistically from the grant-writing paths (`SyncOrgTruth`, `RecordGrant`), throttled to at most once every few minutes. Reads always filter on expiry regardless.
- **Denials are relayed, and only authoritative ones are cached.** A probe answering 404 (or a non-rate-limit 403) is relayed to the caller and cached (~5m) so repeat probing does not hammer GitHub. Transient failures (5xx, 429, rate-limited 403, network) are never cached — they fail only that one request.
- **Fail closed.** A repo whose visibility is unknown is treated as private. A transient `/user` failure with no cached identity verdict fails 503 rather than guessing a principal.
- **The one-truth consequence:** callers do not each pay to warm "their" copy — a webhook or any principal's fetch keeps the copy current. This is for everyone allowed to see it. One user still can never read another's private-repo data: revealing requires GitHub to answered *that* caller.

### App-identity principal (for trusted first-party app callers)

A **GitHub App backend** whose installation tokens rotate hourly will earn grants on a fresh fingerprint principal every hour. Such a caller may instead assert a stable identity by sending its **GitHub App JWT** in an `X-Mirror-Identity` header. The mirror verifies that JWT against GitHub (`GET /app` — unforgeable, since only the app's private key produces a JWT GitHub accepts) and resolves the caller to the principal `app:<id>`. As a result, all of the app's rotating tokens share **one** stable grant set. The `Authorization` token is still used for upstream fetches and probes, so per-repo authorization against GitHub is preserved. Callers that send no identity header keep the default resolution above unchanged — this mode is opt-in and additive.

## REST (cached routes — state-absorbed, rebuilt trimmed)

families of the Fifteen REST route are served from cache (repo-scoped ones behind the show layer above). They do **not** replay GitHub's bytes. The mirror **absorbs the state** contained in the response into structured tables and **rebuilds a trimmed response** from that state, with **every URL field dropped**. This is `url`, anything matching `*_url`, and `_links`. Except a small per-route **pinned allowlist** of consumer-read link fields (the statuses `target_url`, the check-run `details_url`/`html_url`, and the workflow-run `html_url`. This is all rendered by the required-builds hook.

Consumer survey 2026-07-11 — adding one requires a fresh survey). Consumers are first-party tooling (pr-minder etc.) that read state fields only. Hits and misses both serve the rebuilt shape, marked with an `X-GSM-Cache: hit|miss` header. Requests the route cannot model (a non-default `Accept` such as `application/vnd.github.raw`, unknown query params, an unexpected body shape) pass through verbatim, uncached.

Uncacheable reads are also **debounced**: an eligible passthrough `GET` is held for up to `PASSTHROUGH_DEBOUNCE` (default 5s), and every same request arriving. This is in that window shares the upstream call the batch makes. The caller's own credential keys batches. As a result, a response is never shared across tokens. The wait is deliberate — it makes an unmodelable read cost the caller latency instead of costing GitHub a round trip per poll (`X-GSM-Debounce` reports the hold).

- `GET /repos/{owner}/{repo}/contents/{path}` (incl. `?ref=`) — a `200` file (`{type, encoding, size, name, path, content, sha}`), a `200` directory listing (entries as `{type, size, name, path, sha}`), **and a `404`** (rebuilt as `{"message": ..., "status": "404"}`) are all cached. The 404 "config file absent" answer is half the win for config probes. Invalidated per pushed ref (plus the empty-ref default-branch spelling) by `push`, repo-wide by `repository`, with a 24 h TTL backstop. As a result, a missed webhook can never serve stale state forever.
- `GET /repos/{owner}/{repo}/git/commits/{sha}` — rebuilt as `{sha, author, committer, message, tree: {sha}, parents: [{sha}, ...]}`. Immutable content: positive rows have no invalidation and no TTL, bounded only by LRU pruning (so this is the one table that actually grows to the `CACHE_MAX_ROWS` ceiling). **Push webhooks also feed this cache**: the payload's commits (id, tree, message, timestamp, author/committer, with parents derived from the payload's linear chain) are absorbed on delivery, so the common post-push read hits. This is without any GitHub fetch ever having happened. A **404 is cached as an expiring miss marker** (24 h TTL. Pr-minder forever of the shas of the re-reads GC'd test-merge, and each read used to be a fresh upstream. This is 404) that ANY real absorb of the sha clears — a sha that materializes stops answering 404 immediately, and consumers fail open on 404 regardless.
- `GET /repos/{owner}/{repo}/commits` — the commits list (pr-minder's fork-point detection pages this per branch). Each listed commit is absorbed into the same `git_commits_cache` rows as above. A per-query snapshot — keyed by the raw `sha` query value (`''` = default branch), `per_page`, and `page` — stores only the response's ordered shas. As a result, the list rebuilds from the immutable commit rows in the exact order GitHub returned. Item shape: `{sha, commit: {author, committer, message, tree: {sha}}, parents: [{sha}, ...]}`. Only `sha`/`per_page` (1..100)/`page` (1..10) are modeled. Anything else passes through, and only a `200` is absorbed (404/409/5xx relay unstored). Push` and repo-wide by `repository` (the absorbed commit rows stay) with a 24 snapshots of the flushes of the h TTL backstop. The single-commit sub-path `/commits/{sha}` is a different shape and stays passthrough.
- `GET /repos/{owner}/{repo}/compare/{basehead}` — the three-dot `base...head` comparison (pr-minder's empty-PR / open-PR gates run one per branch, every fleet sweep). Rebuilt as `{status, ahead_by, behind_by, total_commits, merge_base_commit: {sha}, commits: [<the commits-list item shape>], files: [{filename, status, additions, deletions, changes, previous_filename?}]}` — the **files array's presence and length are preserved exactly** (consumers read `changed_files = files.length`. An absent array means "unknown, fail open", an empty one means a 0-diff branch), while URL fields and the unread per-file `patch` are dropped. The whole trimmed document is cached per exact basehead (branch names keep their slashes). The compare's commits are also absorbed into `git_commits_cache`. Only the bare query shape with the default JSON `Accept` is modeled — any query param, the `.diff`/`.patch` media types. This is a cross-fork `owner:branch` basehead, or a basehead without `...` passes through. A `200` absorbs the comparison. A **`404` absorbs as an unknown-ref verdict** (served as a cached 404 — the fleet's close-empty pass compares `base...head` for fork PRs whose head ref does not exist in the base repo, re-earning the same 404 every sweep). 5xx relays unstored. A push flushes every comparison naming the pushed ref on either side (so a verdict clears the moment its missing ref is created), `repository` events flush repo-wide, with a 24 h TTL backstop.
- `GET /repos/{owner}/{repo}/commits/{ref}/status`, `GET /repos/{owner}/{repo}/commits/{ref}/check-runs`, and the raw statuses list — under **both** its spellings, the legacy alias `GET /repos/{owner}/{repo}/statuses/{ref}` (what consumers actually send) and the modern `/commits/{ref}/statuses` (one handler, one row space. The status **publish**, `POST /statuses/{sha}`, keeps passing through to GitHub untouched). The `{ref}` is cached **verbatim** — a branch name (slashes and all), a sha, or a tag, not resolved.

  So each spelling is its own snapshot, and `per_page` (1..100) / `page` (1..10) are part of the key (consumers paginate check-runs and statuses at `per_page=100&page=N`). Rebuilt as `{state, sha, total_count, statuses: [{context, state, description, created_at, updated_at}]}`, `{total_count, check_runs: [{id, head_sha, name, status, conclusion, started_at, completed_at, app: {id}, output: {title}, details_url, html_url}]}`, and (statuses list) a bare array of `{context, state, description, target_url, created_at, updated_at}` preserving response order exactly — nullable fields are preserved as null.

  The per-status `target_url` and per-run `details_url`/`html_url` are pinned consumer-read exceptions to the URL-drop rule. `output` is trimmed to exactly `{title}` (never the unbounded summary/text), and the repository/creator objects. Per-item ids are dropped. Check-runs filter params (`check_name`, `filter`, ...) pass through, and only a `200` is absorbed (404/5xx relay unstored). Flushed per payload-named ref (head branches + the sha) by `status`/`check_run`/`check_suite` events, per pushed ref by `push`, and repo-wide by `repository`.

  This is with a 24 h TTL backstop — so snapshots survive exactly while a ref's CI is quiet. This is when watchers re-poll. Other `/commits/*` sub-paths (the single-commit read, `/check-suites`, ...) stay passthrough.
- `GET /repos/{owner}/{repo}/actions/runs?head_sha=<sha>` — the per-sha workflow-runs listing (pr-minder's zombie probe reads only `total_count` at `per_page=1`. Required-builds pages it at `per_page=100`). `head_sha` is **required** for a cacheable shape (the unfiltered listing churns with every run in the repo and is deliberately unmodeled. All other filter params pass through, and deeper `/actions/runs/{id}/...` paths stay passthrough). Whole-doc snapshots per `(owner, repo, head_sha, per_page, page)`. Rebuilt as `{total_count, workflow_runs: [{id, name, head_sha, status, conclusion, html_url, created_at, updated_at, run_started_at}]}` — `total_count` copied verbatim (GitHub's total matching count, not the page length), nullable fields preserved, `html_url` a pinned consumer-read exception. The zero-runs answer is cacheable (it IS the zombie probe's verdict). Flushed per sha by `status`/`check_run`/`check_suite`/ `workflow_job` events (queued jobs included) and repo-wide by `repository`. Run **deletion** emits no webhook, so the h TTL bounds it (consumers fail safe on a stale answer).
- `POST /app/installations/{id}/access_tokens` — the installation-token mint. The bearer here is a GitHub App JWT. The mirror verifies it (`GET /app`) and caches the minted `201` per (app, installation, request-body hash), serving `{token, expires_at, permissions, repository_selection}` until several minutes before the token's real expiry. Invalidated by `installation` / `installation_repositories` events. Unverifiable callers pass through.
- `GET /repos/{owner}/{repo}/pulls` — the open-PR list, rebuilt from the same webhook-maintained global PR state the GraphQL cache uses. Serving a list needs a completeness proof. A per-repo "open-PR list complete" marker: set when a full unfiltered page-1 response is absorbed gats as a result, it, maintained by `pull_request` webhooks. This is (which update the rows, not the marker), and bounded by a 24 h TTL. Only the query shapes its consumers send are modeled. Anything else — and any rebuilt list as long as the requested `per_page` (more pages may exist upstream) — passes through/misses.
- `GET /repos/{owner}/{repo}/pulls/{number}` — a single open PR, served from state only when the cached row carries the REST fields, was **recently touched** (a 24 h staleness backstop, so a missed `closed` delivery cannot serve a stale row forever). This has a **known** `mergeable`. GitHub computes `mergeable` lazily and pr-minder polls this endpoint waiting for it to resolve, so a null/unknown mergeable always misses (the fetch absorbs GitHub's computed answer).

  This is a push to a PR's base or head branch un-resolves the stored value. The cache can never wedge that poll on a stale answer. The push also **remembers the invalidated test-merge sha**: a tip change always changes the test-merge sha. As a result, this is a refetch re-offering that exact sha is a pre-push answer (GitHub's recompute lag) and is stored and served unresolved.

  The poll keeps reaching GitHub, each miss re-triggering the recompute, until GitHub serves a fresh sha (bounded by a ~1 h marker window plus the 24 h row TTL). A fetched CLOSED answer still deletes any cached open row (the open-PR table retains open PRs only). However, the fetched CLOSED answer is absorbed as a rendered whole-document snapshot. The fetched CLOSED answer served from cache thereafter — closed PRs are what every pr-minder drain re-reads.

  This is each such read used to be a fresh passthrough. The closed document is the open rebuild's exact shape plus GitHub's real `merged` and `mergeable` (present, typically null). Every `pull_request`/`pull_request_review` event (so a reopen cannot serve it stale) flushes it, repo-wide by `repository` events, bounded by a 24 h TTL — and deliberately not flushed. This is by pushes (a push cannot mutate a closed PR).

  `Accept: application/vnd.github.diff` (the pr-minder diff read) gets its own flow. A **406 "diff too large" verdict is cached per PR** (the answer pr-minder re-earns before every files-API fallback), while a **200 diff body is never stored** (that will be verbatim byte caching, which the cache model rejects) and relays untouched every time.
- `GET /repos/{owner}/{repo}/pulls/{number}/files` — the PR files listing (pr-minder's fallback when GitHub 406s a too-large unified diff). Whole-doc snapshots per exact page (`owner, repo, number, per_page, page`). The trimmed per-file item is `{filename, status, additions, deletions, changes, previous_filename?, patch?}` with the **presence of `previous_filename` and `patch` preserved exactly** (consumers test `typeof f.patch === 'string'`. GitHub legitimately omits `patch` on binary/oversized files), the per-file blob `sha` and every URL field dropped. `patch` is unbounded, so a rendered document over passes through unstored. Only `per_page` (1..100) and `page` (1..40 — GitHub's files API caps at 3000 files = 30 pages at `per_page=100`, plus margin) are modeled. A non-numeric `{number}` or any other param passes through, and only a `200` is absorbed (an empty page past the end is a valid cacheable answer). Flushed per PR by every `pull_request`/`pull_request_review` event, repo-wide by `push` and `repository`, with a 24 h TTL backstop.
- `GET /repos/{owner}/{repo}/branches` — the branches listing (pr-minder's fork-point detection lists every branch tip per repo, fleet-wide). Whole-doc snapshots per exact page. Trimmed item `{name, commit: {sha}, protected}` with `commit.url`, the protection object, and `protection_url` dropped. Same paging shape guard as the files route. The single-branch `/branches/{branch}` read stays passthrough. Branch create, delete, and tip-move all arrive as pushes, so `push` and `repository` webhooks flush repo-wide, with a 24 h TTL backstop.
- `GET /repos/{owner}/{repo}` — the bare repository read (default-branch lookups and status-only "can I see this repo?" probes). No snapshot table and no TTL: the response is rebuilt straight from the `repos` truth row the webhooks, fleet sync, and consistency check already maintain. Truth freshness is the freshness model, exactly as the GraphQL route serves truth. Served only when the row can answer fully (known visibility — unknown fails closed — a known default branch, and the canonical full name). Otherwise the fetch's absorbed `200` heals the row. Trimmed rebuild: `{name, full_name, owner: {login}, private, visibility, default_branch, archived, disabled}`. Query params and `HEAD` requests pass through. A non-200 relays unstored.
- `GET /repos/{owner}/{repo}/installation` — the App-level "which installation covers this repo" lookup. App-JWT verified like the token mint, cached per app as `{id, account{login,type}, repository_selection, app_id, app_slug, target_type}`, flushed by `installation`/`installation_repositories` events plus a 24 h TTL.

Cached state is global truth, revealed per caller by the show layer. Caps + LRU pruning bound each response-cache table (per-table row ceiling `CACHE_MAX_ROWS`, default 1,000,000). (Only the App-credential caches — minted tokens and repo-installation lookups — stay keyed by the app principal: a minted token is per-credential, not shared truth.)

(The byte-identity rule now applies only to the GraphQL org-repos route below. The trimmed rebuild contract above is the operator-chosen model for cached REST routes.)

## GraphQL

- `POST /graphql` — the org-repos query (an `organization { repositories { ... pullRequests ... } }` shape) is served from the cache. An identity test against a recorded GitHub response) assembls this. Any other GraphQL query is forwarded to GitHub (see passthrough below).

## GitHub passthrough (everything else)

Any request the mirror does not serve from cache is **transparently forwarded to GitHub** (`https://api.github.com`) and returned **uncached**. This makes the mirror a drop-in replacement for `api.github.com`: the endpoints above are served fast from the global cache. Every other endpoint (issues, releases, notifications, an unrecognized GraphQL query, ...) still works. A 2xx passthrough under `/repos/{owner}/{repo}/...` also refreshes the caller's grant — GitHub just proved their access. Mutating methods are recorded as `write` in the request log (they are proxied because they mutate, not because caching failed).

- The caller's `Authorization` header is forwarded unchanged — the mirror never substitutes its own `GITHUB_TOKEN` — and a forwarded request **still requires a token** (`401` otherwise). As a result, the mirror is never an open, unauthenticated relay.
- Responses are passed through verbatim, including status, body, and headers such as `Link` (pagination) and `X-RateLimit-*`. The mirror's own CORS headers are authoritative. GitHub's duplicate `Access-Control-Allow-*` are stripped while `Access-Control-Expose-Headers` is preserved so browsers can read those rate-limit/link headers.
- This path is uncached: it never reads or writes the freshness store.

## OAuth relay (github.com login endpoints)

GitHub's login endpoints live on `github.com` — not `api.github.com` — and send no CORS headers, so a purely client-side app (e.g. the repo-nightmare PR viewer) cannot call them directly. The browser blocks the JS from reading the response and the login silently fails. The mirror stands in as the CORS-correct relay, forwarding the body verbatim and returning GitHub's response with the mirror's CORS headers. These routes carry **no** bearer token (the body is the credential — an OAuth `client_secret`, or just a public `client_id` for the device flow). As a result, they are unauthenticated and target fixed `github.com` URLs — not the `api.github.com` passthrough.

- `POST /login/oauth/access_token` — the OAuth "exchange code for token" request. This is also the device flow's **polling leg**: a `{client_id, device_code, grant_type: "urn:ietf:params:oauth:grant-type:device_code"}` poll passes through unchanged (the body is opaque bytes to the relay).
- `POST /login/device/code` — starts a device authorization sign-in (RFC 8628, the same flow `gh auth login` uses): relays the `{client_id, scope}` request and returns GitHub's `device_code`/`user_code`/`verification_uri` JSON. This is after which the client polls the `access_token` route above.

## Webhook

- `POST /webhook` - receives GitHub webhook events. The handler writes each verified delivery to the inbox, synced to disk, and answers. A worker applies it to the cache, the client lane ahead of Actions churn (docs/webhooks/dispatch.md, "Receipt and apply", and docs/webhooks/lanes.md). A success means the delivery is kept, and will be applied across a start:
  - `202 Accepted`, `queued` - recorded. The worker applies it
  - `202 Accepted`, `duplicate` - this `X-GitHub-Delivery` is already recorded, and is not applied twice
  - `500`, `error` - the inbox can not record it. It lands in GitHub's failure log, and the replayer asks for it again

  The disposition is returned in the JSON body and an `X-GSM-Disposition` header. What the apply did is recorded in the dashboard's webhook log (see below).

  Besides the PR/check/push/label/repository events that feed global truth, the mirror also tracks **`workflow_job`** deliveries (`in_progress` and `completed`. `queued`/`waiting` are dropped as `ignored`). Recording state of the GitHub Actions job as it happens in a **global** table read via the admin-only `GET /api/jobs`. The GitHub App must be **subscribed to the `workflow_job` event** in its settings to receive these. Expect high volume in a CI-heavy org — each delivery costs one inbox row and one short transaction.

## Subscriber notifications (`/_mirror/subscriptions`)

Consumers that receive their own GitHub webhooks and immediately query the mirror **race its ingestion**. Their delivery can arrive before the mirror's, and the read returns pre-event state. Subscriber notifications remove the race — the mirror POSTs a compact, HMAC-signed JSON notification to your endpoint **after** its worker applied the delivery. This is to global truth, so keying your reads off the mirror's notification always reads post-ingest state.

The top-level **`/_mirror/*` prefix is the reserved mirror-native namespace**: GitHub's API has no underscore-prefixed top-level paths. GitHub's API registered routes always win over the passthrough proxy. As a result, nothing under `/_mirror/*` can ever collide with proxied GitHub traffic.

**Registering** (same bearer-token auth as every data route. The principal your token resolves to owns subscriptions, so an app using `X-Mirror-Identity` keeps its subscriptions across token rotations):

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

Subscription JSON: `{id, url, repos, events, active, consecutive_failures, disabled_reason, created_at, updated_at, last_success_at, last_failure_at, last_error}`. The `secret` is **never** returned by any response. `url` must be absolute https with no userinfo (plain http is allowed only for loopback hosts, so local receivers work) and must not be a private/link-local IP literal. `events` are GitHub event names (`push`, `pull_request`, ...). Each principal may hold a bounded number of subscriptions (409 beyond).

**Deliveries** are `POST`s — **GitHub's exact signature scheme**, so the webhook verification code. You already have works unchanged. Example payload (fields absent from the event are omitted):

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

PR events carry `pr_number` + the head `sha`. Pushes carry `ref` + the after `sha`. Status/check/workflow-job events carry the commit `sha`. **Correlation note:** `github_delivery` is the `X-GitHub-Delivery` GUID **the mirror received** — it is NOT the GUID GitHub sent *you* for the same event (GitHub mints one per receiver). Correlate on `owner`/`repo` plus the identifier fields instead.

Delivery semantics:

- Only dispositions **`applied`** and **`invalidated`** notify (`ignored`/`error` do not), and only deliveries that resolve to a single `owner/repo` — repo-less events (`installation`, `installation_repositories`) never notify.
- Any **2xx** from your endpoint counts as delivered. A failed delivery is retried a bounded number of attempts (short backoff, ~8s per try). A terminal failure increments `consecutive_failures` and stamps `last_failure_at`/`last_error`.
- At **10 consecutive terminal failures** the subscription is **auto-disabled** (`active=false`, `disabled_reason` set). Re-enable it after fixing your endpoint with `PATCH {"active": true}`, which also resets the failure counter.
- Notifications are fanned out **after** the webhook response to GitHub — subscriber endpoints can never slow the mirror's ingestion.

**Authorization (reveal-gated, fail closed):** a subscription is notified about a repo only if its principal can read that repo through the show layer's non-probing fast paths. This is at delivery time. The repo is **public** in global truth, or the principal holds a **live grant**. There is no per-notification probe (no caller token exists at delivery time). Unknown visibility reads as private, so a private repo's activity never reaches a principal that has not proven access. Earn the grant the normal way (an org list-sync or any revealed read with your token) before expecting private-repo notifications.

**Persistence:** subscriptions are service **config**, not cache. They live in their own SQLite file (`SUBSCRIPTIONS_DB_PATH`, default derived from `DB_PATH`: `github-mirror.db` → `github-mirror-subscriptions.db`) which **survives the cache DB's schema nukes** and every deploy.

**Operator view:** admin-only `GET /api/notifications` (dashboard session auth) returns in-memory delivery counters (`delivered` / `failed` / `gated` / `auto_disabled`), the recent delivery attempts (bounded ring. Resets on start), and every subscription with its principal (secrets redacted). JSON only — there is no dashboard tab.
