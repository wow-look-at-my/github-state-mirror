# The dashboard: authz model and admin views

Extracted verbatim from CLAUDE.md, which keeps the one-line index entry. The
dashboard's authorization model (GitHub OAuth + login allow-list) is
deliberately distinct from the data API's bearer-token + reveal model.

**Dashboard = separate authz model** — the web dashboard (`internal/api/dashboard.go`, served at `/`) authenticates a human via **GitHub OAuth** and authorizes by **login** (session cookie), which is deliberately distinct from the data API's bearer-token + reveal model. OAuth callback failures (invalid/expired state, missing code, a failed code-for-token exchange or `GET /user`) render a small fixed-content "Sign-in failed" retry page at **400** with every branch slog-logged — never a 5xx, because Cloudflare replaces origin 5xx bodies with its own bare error page (the 2026-07-19 sign-in dead end). It reports the GLOBAL truth totals (one cache, one truth — same numbers for everyone) plus per-**principal** reveal-layer standing: live grant count, org-sync freshness, recent refreshes. A normal user sees their own principal(s); logins in `ADMIN_LOGINS` (default `PazerOP`) see every known principal. The `actor_identities` table maps principal→display name (a user's login, an app's slug, or an installation's account login) for this labeling only — written by `requireAuth`, the self-verifying app-JWT routes (token mint, repo installation), and the background refresher's installations listing; names come only from verification calls that already happen, never extra GitHub calls, and every surface falls back to the bare key. Admins additionally get: the **webhook delivery log** (`GET /api/webhooks`, "Webhooks" tab, served from `dashboard_webhooks.go` — dispositions are `applied`/`invalidated`/`ignored`/`error`; there is no `skipped`. The response also carries `missing_subscriptions`: the `ghdata.RequiredWebhookEvents` the App is NOT subscribed to, each with what degrades without it, rendered as a banner above the table. A missing App event subscription is this service's quietest failure — the caches that need it just re-fetch forever, or serve within their TTL, and nothing else anywhere says so — so it is reported rather than left to be inferred from an absence. The answer comes from GitHub (`AppAuthenticator.SubscribedEvents`, `GET /app` over the App's own JWT), and the events GitHub delivers to every App unconditionally (`ghclient.AlwaysDeliveredEvents`: `installation`, ...) are never reported, since they appear in no App's list. It must NEVER be inferred from the delivery log again: that log is bounded by ROW COUNT, so on a CI-heavy fleet it spans minutes, and the inference then reports every low-frequency required event (`repository`, `label`) as unsubscribed permanently while they are correctly configured — a banner that is wrong on every page load, which is how a real missing subscription gets skimmed past. When the answer is unavailable (no App configured, or the lookup failed) the field is omitted entirely: "I could not determine this" must not render as "these are missing"), the **request-activity log** (`GET /api/requests`, in-memory `requestLog`, "Requests" tab — `hit`/`miss`/`passthrough`/`write`/`error` with per-disposition totals, plus **route-shape groups**: `requestgroups.go`'s `normalizeRoute` folds each path onto its shape (`/repos/{owner}/{repo}/compare/{basehead}`, ...) and the log keeps bounded cumulative per-group disposition counters — since-restart like the totals, NOT windowed by the recent ring — which the tab renders as two tables above the flat history, "Top requests" and "Top uncached requests" (passthrough > 0, the caching candidates). Every passthrough also records **WHY** it was forwarded — a CLOSED, compile-time reason vocabulary (`requestlog.go`: `unmodeled-query`, `unmodeled-accept`, `unmodeled-path`, `unrouted`, `unrouted-method`, `unverified-identity`, `unmodeled-response`, `graphql-forward`), set by `h.passthrough(w, r, reason)` at each cached route's bail-out (never by calling `ghProxy.ServeHTTP` directly) and by `taggedProxy` on chi's NotFound/MethodNotAllowed. Reasons are group-counter keys, so like the timeline's lanes they may NEVER be derived from caller-controlled input; only passthroughs carry one (`recordFull` clears a stray hint on any other disposition). Each group also samples one passthrough's query-parameter **NAMES** (`pass_query`, e.g. `per_page,status`) — never VALUES, which can carry a credential. This is what turns a volume number into an actionable gap: the actions/runs route's dominant `unmodeled-query`/`?status,per_page` traffic was the GHA runner coordinator's queued-backlog poll, written off for a long time as inherently uncacheable and now served from webhook-maintained `workflow_runs` truth — every remaining reason is likewise a gap to close, never a verdict; the payload also carries the SQLite DB's on-disk size — `db_size_bytes`/`db_wal_size_bytes`, `DB_PATH` + its `-wal` sidecar statted per request, omitted when missing — shown on the tab's summary line), the **rate-limit view** (`GET /api/ratelimit`, "Rate limit" tab — `{live, observed, note?}`: `live` is the GitHub App's per-installation `GET /rate_limit` poll, `observed` is the `internal/ratemeter` snapshot of passively recorded `X-RateLimit-*` headers per (identity, resource); with no App configured — or a failed poll — the endpoint answers 200 with an empty `live`, a `note`, and the observed data, never a bare 503), the **Timeline chart** (`GET /api/timeline?since=<id>`, "Timeline" tab — the `internal/reqtimeline` ring rendered as a swimlane chart of EVERY exchange the mirror participates in: one lane per webhook event type (plus the fixed rejected-delivery lanes) / per route shape (inbound requests AND the mirror's own upstream calls, distinguished by disposition and actor) / the "⇒ notify" outbound lane — real measured durations, ms-scale events as native instant pips; the tab's `<gsm-timeline>` element is created ONCE and kept alive across refreshes — it polls with the `since` cursor and `mergeData`s in place, deliberately breaking the other tabs' wipe-and-rebuild convention because the canvas holds viewport state; the `<timeline-view>` component itself is imported at RUNTIME from js-snippets' buildhost library site (`https://sites.pazer.build/js-snippets/branch/library/ui/timeline-view.js` — replaced the quota-dead GitHub Pages deploy 2026-07-20), never vendored — fix component bugs upstream — with a forever-retrying, cache-busted dynamic import and a "chart loading…" note while unreachable), and `GET /api/jobs` (workflow jobs; no dashboard tab yet). The request log, the rate meter, and the timeline ring are **in-memory** (live views, not audit logs) so they reset on restart.

## `handleJobs` (`GET /api/jobs`)

Returns recent GitHub Actions jobs recorded from `workflow_job` webhooks:
running jobs first (newest started first), then completed jobs (newest
completed first). Like the webhook log, the job table is global (it spans
every repo/tenant), so it is restricted to admins. `limit` caps the row count
(default 100, max 500).

### Storage (`internal/ghdata/workflow_jobs.go`)

GitHub Actions job state fed by `workflow_job` webhooks. Like the webhook
delivery log, this data is GLOBAL (not actor-scoped): it is webhook-fed
operational telemetry with no per-credential fetch path -- a job's state only
ever arrives via the HMAC-verified delivery, never through a caller's token,
so there is no credential to partition by.

`workflowJobRetention` bounds the table's growth: completed jobs older than
this on BOTH clocks -- the job's own `completed_at` AND the row's `updated_at`
(when the last webhook was applied) -- are pruned after each upsert (one
cheap indexed DELETE). The `updated_at` key is what keeps the upsert's
out-of-order guard sound: the row is the guard's only memory, so it must
survive a full retention window after the last delivery touched it, not
merely after the event's own timestamp (a replayed old completed event would
otherwise be recorded and pruned by the same call, and a late `in_progress`
would then resurrect the job as a phantom running row). Jobs still in
progress are never pruned. Two weeks keeps enough history to be useful while
a CI-heavy org's job volume stays bounded; no config knob on purpose (this is
observability, not source-of-truth).

## `handleRequests` (`GET /api/requests`)

Returns recent data-API requests and their cache disposition (hit / miss /
passthrough). Like the webhook log it spans every actor/tenant, so —
consistent with the admin-only "all scopes" view — it is admin-only. The
payload also carries the SQLite database's on-disk size (statted fresh per
request) so the tab's summary shows the cache's real footprint.

## `dbFileSizes`

Reports the on-disk sizes of the SQLite database file and its `-wal` sidecar
(bytes written ahead of a checkpoint — part of the real footprint). One
`os.Stat` each — cheap enough per request, no caching. A missing file or stat
error yields 0 (the field is omitted from the JSON), never a failure: the
request log must render regardless.

## `groupKinds`

Folds per-(kind,state) rows into one entry per resource kind, with a map of
state -> count and the most recent fetch time across that kind. When a kind
has an errored resource, the first captured error message (and its key) is
attached so the dashboard can show why it failed.
