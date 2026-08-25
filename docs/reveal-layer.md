# One global truth, revealed by permission

The load-bearing security section: storage is global, authorization is a read-time gate. Extracted verbatim from CLAUDE.md.

- **One global truth, revealed by permission (replaces per-actor isolation)** — see "The global-truth model" above; this is the load-bearing security section. The principal is resolved in `requireAuth` (`internal/api/router.go`): a user token resolves via `ghclient.ResolveTokenIdentity` (`GET /user`, cached per token) to `user:<numeric id>` (stable across renames, shared by all of that user's tokens); a token that is definitively NOT a user (non-rate-limit 403 / 404 on `/user`) falls back to the SHA-256 token fingerprint; a transient `/user` failure with no cached verdict fails 503 (never a silent guess). The security boundary is **the reveal layer, not storage**: one user can never read another's private-repo data because revealing `owner/repo` to a principal requires the repo to be public, a live grant (earned from GitHub answering THAT principal), or a live probe with THAT principal's own token. Public repos are served to any authenticated caller. Fail-closed rules that must not regress: unknown visibility = private; deny verdicts cache only authoritative answers (a rate-limited 403 must never pin a legitimate caller out for the TTL); grants expire (~24h) so revoked GitHub access converges; `SyncOrgTruth` REPLACE-syncs list_sync grants (a repo GitHub stopped returning loses its grant). Data endpoints reject tokenless requests with 401. There is **no static service token**: the **GitHub App** (`ghclient.AppAuthenticator`) is the service's only credential, used for background work (periodic refreshes and the consistency check), never to serve requests; the periodic refresher mints a short-lived access token per installation and runs as the stable `app-installation:<id>` principal (see `sync.AppSessions`), so its syncs both refresh global truth and hold that principal's grants. The refresher syncs each installation's ACCOUNT by name (`Session.Owner` → `InvalidateAndRefresh`, which seeds the freshness row itself — a fresh installation is synced on the first cycle) via the owner-agnostic `GetOwnerData`; the fetcher picks the query by principal, so lazy callers keep the identity-locked org query. The owner query carries per-repo `visibility`, so every refresher sync stamps it into truth (only when the fetch knows it — an empty value never overwrites, so the locked org-query path can't blank a known visibility) and `visibility_unknown` can no longer accumulate for fleet-synced owners. (The old `RefreshAllOfKind` shape only re-fetched rows that already existed under the actor — and nothing ever created one, so fleet sync silently never ran.) `PeriodicRefresher.Start` runs the FIRST fleet cycle **immediately at startup**, then ticks on `REFRESH_INTERVAL` (default 6h) — a bare ticker's first fire sat a full interval away, and under a deploy cadence shorter than that (schema-bump deploys also nuke the freshness markers) no cycle ever completed in production. `refreshAll` checks `ctx.Err()` between sessions, so a shutdown mid-cycle WARN-logs how many owners were left instead of silently truncating the fleet.

## The four-step decision (public / grant / deny cache / probe)

The truth store is global (one row per resource; webhooks and any caller's
fetches all feed it), so serving cached state must be gated per caller.
GitHub itself is the permission oracle -- the mirror never invents an
authorization model, it only caches GitHub's own answers:

1. PUBLIC fast path: the repo's webhook/REST-learned visibility is
   'public' -> any authenticated principal may read its cached state. A
   repo whose visibility is unknown ('', e.g. seeded only by the
   identity-locked GraphQL fetch, which cannot carry visibility) is
   treated as PRIVATE -- fail closed.
2. GRANT: the principal holds an unexpired access_grants row for the
   repo, earned from GitHub's own answers to that principal's requests
   (an org list-sync with their token, or an earlier probe).
3. DENY VERDICT: GitHub authoritatively told this principal "no" (404 /
   non-rate-limit 403) for this exact resource within the deny TTL ->
   serve that answer again without asking GitHub.
4. PROBE: otherwise ask GitHub: GET /repos/{owner}/{repo} with the
   caller's own token (buildhost's canAccessRepo pattern). 200 proves
   access -> absorb the (canonical, visibility-carrying) repository
   object into truth, record a grant, proceed. 404/authoritative-403 ->
   record a deny verdict, relay the answer. Transient failures (5xx,
   429, rate-limited 403, network) are relayed but NEVER cached -- a
   hiccup must not pin a caller out (or in).

The probe costs one upstream call on a principal's FIRST touch of a
non-public repo (per grant TTL); it also heals unknown-visibility rows,
since the probe response carries visibility for everyone's benefit.

## `KindOrgRepos` (`internal/sync/registry.go`)

A PRINCIPAL's org list-sync marker (actor = principal, key = org login):
freshness of that principal's grant set for the owner. The fetch refreshes
global truth as a side effect. (The `/pulls` list's completeness marker lives
in its own table, `pulls_list_cache` -- that route absorbs the caller's own
request rather than running a fetcher.)

## Trusted-app mode (X-Mirror-Identity)

A caller may assert a stable identity with a GitHub App JWT in
X-Mirror-Identity. We verify it against GitHub (GET /app — unforgeable, since
only the app's private key produces a JWT GitHub accepts) and partition that
caller by the app, NOT by the token fingerprint. This lets a first-party app
whose installation tokens rotate hourly share one warm cache bucket, while the
Authorization token is still used for upstream fetches so per-repo
authorization is preserved. Callers without this header keep the fingerprint
isolation, so untrusting multi-tenant use is unaffected. (Distinct from the
background refresher's app-installation:<id> partition: that is the mirror as
its own app; this is an external app caller tagging its data-API requests.)
