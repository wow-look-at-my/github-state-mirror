package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/notify"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// requireAuth enforces that every data request carries a usable GitHub token.
// It resolves the token's identity against GitHub (rejecting absent, malformed,
// or revoked credentials with 401), injects the token into the request context,
// and scopes all cache operations to a per-USER partition.
//
// The cache partition (actor) is "user:<numeric GitHub user id>" — 1 GitHub
// user == 1 cache scope (operator decision, 2026-07-03). All of a user's
// tokens (rotating sandbox PATs, OAuth logins, narrow and broad PATs alike)
// share one warm, webhook-fed bucket, so a user is never isolated from
// themselves just because their tokens rotate. The numeric id (not the login)
// keys the bucket because ids survive login renames and are never recycled.
// Accepted trade-off: ANY token of a user reads what any of that user's tokens
// cached, including private-repo data cached by a broader-scoped token.
// DISTINCT users remain fully isolated from each other, and requests must
// never fall through to the service's own credentials (the GitHub App used for
// background refreshes), which may have far broader access than the caller.
//
// A token that is definitively NOT a user — GET /user answers 403/404, e.g. a
// GitHub App installation token — keeps the old per-token fingerprint
// partition (and the verdict is cached per token). When the identity cannot be
// resolved at all (network error, 5xx, rate limit) and no verdict is cached,
// the request FAILS with 503: mis-partitioning is worse than a failed request,
// so there is no silent fingerprint fallback for a token that might belong to
// a user.
func requireAuth(gh *ghclient.Client, record identityRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				http.Error(w, "unauthorized: missing Authorization header", http.StatusUnauthorized)
				return
			}
			ctx := ghclient.WithToken(r.Context(), token)

			// Trusted-app mode: a caller may assert a stable identity with a
			// GitHub App JWT in X-Mirror-Identity. We verify it against GitHub
			// (GET /app — unforgeable, since only the app's private key produces
			// a JWT GitHub accepts) and partition that caller by the app, NOT by
			// the token fingerprint. This lets a first-party app whose
			// installation tokens rotate hourly share one warm cache bucket,
			// while the Authorization token is still used for upstream fetches so
			// per-repo authorization is preserved. Callers without this header
			// keep the fingerprint isolation below, so untrusting multi-tenant
			// use is unaffected. (Distinct from the background refresher's
			// app-installation:<id> partition: that is the mirror as its own app;
			// this is an external app caller tagging its data-API requests.)
			if idJWT := r.Header.Get("X-Mirror-Identity"); idJWT != "" {
				ident, err := gh.VerifyAppIdentity(ctx, idJWT)
				if err != nil {
					slog.Warn("verify app identity failed", "error", err)
					http.Error(w, "unauthorized: could not verify identity assertion", http.StatusUnauthorized)
					return
				}
				actorKey := fmt.Sprintf("app:%d", ident.ID)
				ctx = actor.WithActor(ctx, actorKey)
				if ident.Slug != "" {
					// The app slug is GitHub-verified (GET /app answered it):
					// carry it as the principal's display name so downstream
					// surfaces (request log, rate meter, logs) can show it.
					ctx = actor.WithName(ctx, ident.Slug)
				}
				record(ctx, actorKey, ident.Slug)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Resolve the credential's identity with GitHub up front (cached
			// per token, including the definitive not-a-user verdict). A user
			// token lands in that user's shared bucket; a non-user token keeps
			// per-token fingerprint isolation. An unresolvable identity is a
			// hard failure — see the function comment.
			ident, err := gh.ResolveTokenIdentity(ctx)
			if err != nil {
				if errors.Is(err, ghclient.ErrBadCredential) {
					slog.Warn("resolve token identity: bad credential", "error", err)
					http.Error(w, "unauthorized: could not validate GitHub credential", http.StatusUnauthorized)
					return
				}
				slog.Warn("resolve token identity failed; refusing to guess a cache partition", "error", err)
				http.Error(w, "service unavailable: could not resolve the credential's GitHub identity (required for cache partitioning); please retry", http.StatusServiceUnavailable)
				return
			}
			actorKey := ghclient.Fingerprint(token)
			if ident.IsUser {
				actorKey = fmt.Sprintf("user:%d", ident.ID)
			}
			ctx = actor.WithActor(ctx, actorKey)
			if ident.IsUser && ident.Login != "" {
				// The login came from GitHub's own GET /user answer: carry it
				// as the display name. Non-user tokens have no name.
				ctx = actor.WithName(ctx, ident.Login)
			}
			// Remember the actor->login mapping so the dashboard can group a
			// user's scope by login. A non-user token has no login and is
			// skipped by the recorder.
			record(ctx, actorKey, ident.Login)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// identityRecorder persists an actor->login mapping for the dashboard.
type identityRecorder func(ctx context.Context, actorFP, login string)

// newIdentityRecorder returns a recorder that upserts the actor->login mapping,
// debounced to at most once per minute per actor so the hot request path does
// not write on every call.
func newIdentityRecorder(store *ghdata.Store) identityRecorder {
	var lastWrite sync.Map // actorFP -> time.Time
	return func(ctx context.Context, actorFP, login string) {
		if login == "" {
			return
		}
		if v, ok := lastWrite.Load(actorFP); ok {
			if t, ok := v.(time.Time); ok && time.Since(t) < time.Minute {
				return
			}
		}
		lastWrite.Store(actorFP, time.Now())
		if err := store.RecordActorIdentity(ctx, actorFP, login); err != nil {
			slog.Warn("record actor identity failed", "error", err)
		}
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" or
// "Authorization: token <token>" header, returning "" when absent.
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	token = strings.TrimPrefix(token, "token ")
	return strings.TrimSpace(token)
}

func NewRouter(
	mgr *freshness.Manager,
	store *ghdata.Store,
	webhookSecret string,
	dispatcher *syncpkg.WebhookDispatcher,
	gh *ghclient.Client,
	allowedOrigins []string,
	authSvc *auth.Service,
	baseURL string,
	checker *syncpkg.ConsistencyChecker,
	meter *ratemeter.Store,
	notifier *notify.Notifier,
	dbPath string,
	timeline *reqtimeline.Recorder,
	debouncer *Debouncer,
	app *ghclient.AppAuthenticator,
) http.Handler {
	r := chi.NewRouter()
	// First: stamp every request's receipt time, so any record site can put a
	// real end-to-end duration on the Timeline chart.
	r.Use(stampRequestStart)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	// CORS so browser clients on other origins (e.g. the repo-nightmare PR
	// viewer on GitHub Pages) can call the API. Mounted before the auth group
	// so preflight OPTIONS is answered without a token.
	r.Use(corsMiddleware(allowedOrigins))

	// In-memory record of data-API requests (hit/miss/passthrough) for the
	// dashboard's "Requests" view. Every observed request is also mirrored,
	// timed end-to-end, onto the Timeline chart.
	reqlog := newRequestLog()
	reqlog.timeline = timeline

	// Captured SHAPE of uncached traffic (shapes.go): the query-parameter
	// names callers send and the key/type outline of what GitHub answers,
	// sampled at most once per route shape per window. It is what the
	// admin-only implementation brief (GET /api/brief) is assembled from —
	// the input for modeling the next cached route, which the request-group
	// counters alone cannot supply.
	shapes := newShapeStore()

	// Transparent GitHub passthrough for anything the mirror does not serve
	// itself. Built from the same base URL the cache fetchers use, so forwarded
	// requests reach the same upstream (a fake server in tests). Wrapped so every
	// proxied request is recorded as a passthrough — and timed into the
	// timeline ring for the dashboard's "Timeline" chart.
	//
	// The debouncer sits BETWEEN the recorder and the proxy: every inbound
	// request is still recorded and timed individually (a waiter's duration
	// honestly includes the hold), while the coalesced batch makes at most one
	// call through the proxy itself. Nil (window <= 0) leaves the chain
	// untouched.
	debouncer.attach(reqlog, timeline)
	ghProxy := recordPassthrough(debouncer.Wrap(newGitHubProxy(gh.BaseURL(), meter, func(resp *http.Response) {
		// A 401/403 on a passthrough call carrying a minted installation
		// token invalidates that mint (see invalidateMintOnAuthFailure).
		// resp.Request is the outbound clone carrying the inbound headers.
		if resp.Request != nil {
			invalidateMintOnAuthFailure(resp.Request.Context(), store, bearerToken(resp.Request), resp)
		}
	})), reqlog, shapes)

	// One debounced principal->name recorder shared by requireAuth and the
	// self-verifying app-JWT routes (token mint, repo installation), so every
	// GitHub-verified identity lands in actor_identities.
	recordIdentity := newIdentityRecorder(store)

	h := &handlers{mgr: mgr, store: store, ghProxy: ghProxy, reqlog: reqlog, gh: gh, upstream: &http.Client{}, meter: meter, recordIdentity: recordIdentity, timeline: timeline, shapes: shapes}

	// Web dashboard: static page, GitHub OAuth login, and the cache-stats API.
	// Authorized by session cookie (login), distinct from the data API below.
	// dbPath (DB_PATH) lets the Requests view report the DB's on-disk size.
	// The subscription check reads the App's own answer, so it exists only
	// when an App is configured; nil leaves the Webhooks tab reporting the
	// deliveries and nothing about subscriptions.
	var appEvents func(context.Context) ([]string, error)
	if app != nil {
		appEvents = app.SubscribedEvents
	}
	newDashboard(authSvc, store, baseURL, reqlog, checker, meter, notifier, dbPath, timeline, shapes, appEvents).routes(r)

	// Webhook endpoint — authenticated by HMAC signature (X-Hub-Signature-256),
	// not a user token, so it sits outside the requireAuth group. After each
	// synchronous dispatch the subscriber notifier fans the outcome out to
	// registered endpoints (non-blocking; nil keeps the feature inert). Each
	// verified delivery's full handling duration is also recorded into the
	// timeline ring (the dashboard's "Timeline" chart).
	r.Post("/webhook", webhook.Handler(webhookSecret, dispatcher, timelineDeliveryRecorder(timeline), notifier))

	// The update-liveness contract docker-updater probes on the container's own
	// port, reading only the status code. Outside requireAuth deliberately: the
	// prober carries no bearer token, and an unregistered path here does not
	// 404 -- it falls through to the GitHub passthrough, which answers 401 for a
	// tokenless request. The contract reads any non-404 as "implemented", so
	// leaving these unregistered reports the container as implemented and
	// permanently unhealthy rather than unconfigured.
	registerUpdateChecks(r, store, mgr)

	// GitHub OAuth relays for browser clients. A purely client-side app cannot
	// POST to github.com's login endpoints directly (they send no CORS
	// headers); the mirror relays them with correct CORS. They carry no bearer
	// token (the body is the credential — an OAuth client secret, or just a
	// public client_id for the device flow), so they sit outside requireAuth,
	// and they target github.com — not the api.github.com passthrough.
	// access_token is the code-for-token exchange AND the device flow's
	// polling leg; device/code starts an RFC 8628 device sign-in.
	r.Post("/login/oauth/access_token", h.oauthAccessToken)
	r.Post("/login/device/code", h.oauthDeviceCode)

	// Installation-token mint cache and the repo-installation lookup.
	// Registered OUTSIDE requireAuth: the bearer token on both is a GitHub App
	// JWT (it cannot resolve GET /user), so each handler verifies it itself
	// via VerifyAppIdentity and partitions by the verified app id.
	// Unverifiable callers are forwarded unchanged.
	r.Post("/app/installations/{id}/access_tokens", h.cachedInstallationToken)
	r.Get("/repos/{owner}/{repo}/installation", h.cachedRepoInstallation)
	// The OWNER-level installation lookups answer the same object for an
	// account instead of a repository, on the same App-JWT terms and in the
	// same row space (under a sentinel repo value -- see
	// respcache_installation.go). Two registrations because they are two
	// questions: an account can answer one and 404 the other.
	r.Get("/orgs/{org}/installation", h.cachedOwnerInstallation(ownerInstallScopeOrg, "org"))
	r.Get("/users/{username}/installation", h.cachedOwnerInstallation(ownerInstallScopeUser, "username"))

	// Data endpoints — every request must carry a valid GitHub token, and all
	// cache access is scoped to that credential's partition (the requireAuth
	// actor): the token's GitHub user ("user:<id>"), app:<id> for verified
	// X-Mirror-Identity callers, or the token's fingerprint for non-user
	// tokens.
	//
	// The cache contract is three-tiered (see CLAUDE.md): the org-repos GraphQL
	// query is served byte-identical to GitHub (identity-test-locked); the
	// cached REST routes below ABSORB the response's state and REBUILD a
	// TRIMMED body with every URL field (url, *_url, _links) dropped; and
	// everything else falls through to the verbatim passthrough, uncached.
	r.Group(func(r chi.Router) {
		r.Use(requireAuth(gh, recordIdentity))

		// Subscriber-notification CRUD (subscriptions.go), under the RESERVED
		// mirror-native /_mirror/* namespace (GitHub has no underscore-prefixed
		// top-level paths, and registered routes beat the NotFound passthrough,
		// so this can never collide with proxied GitHub traffic). Principal-
		// scoped via requireAuth like every data route; not a repo read, so no
		// reveal gate and no request-log entry.
		(&subscriptionsAPI{notifier: notifier}).routes(r)

		// GraphQL endpoint (only the org-repos query shape is cached; everything
		// else h.graphql forwards to the passthrough).
		r.Post("/graphql", h.graphql)

		// Cached REST routes (respcache.go): repo contents (200 file/dir AND
		// the 404 "config absent" answer; push/repository webhooks invalidate)
		// and immutable git commits (also absorbed from push payloads).
		r.Get("/repos/{owner}/{repo}/contents/*", h.cachedContents)
		r.Get("/repos/{owner}/{repo}/git/commits/{sha}", h.cachedGitCommit)

		// Cached ref lookup (respcache_gitrefs.go): "where does this branch
		// point right now". Greedy wildcard -- a ref path is at least
		// heads/<name> and branch names carry slashes -- stored VERBATIM, so
		// each spelling of a ref is its own row. Create, delete, and tip-move
		// all arrive as a push naming the ref, which flushes every spelling;
		// the 404 absent-ref verdict is absorbed too (sweeps re-poll deleted
		// heads forever) and cleared by the push that recreates the ref.
		r.Get("/repos/{owner}/{repo}/git/ref/*", h.cachedGitRef)

		// Cached commits LIST (respcache_commits.go): per-page sha snapshots
		// over the same git_commits_cache rows, flushed by push/repository
		// webhooks.
		r.Get("/repos/{owner}/{repo}/commits", h.cachedCommitsList)

		// The /commits/* subtree dispatcher (respcache_commitci.go): a tail
		// ending in /status (the combined commit status), /check-runs, or
		// /statuses (the raw statuses LIST) is a cached commit-CI route --
		// the suffix anchor is what lets the ref carry slashes, which a
		// single-segment {ref} parameter could never match. Snapshots are
		// keyed by the VERBATIM ref (branch, sha, or tag; never resolved) +
		// kind + per_page/page, and flushed per payload-named ref by
		// status/check_run/check_suite (CI moved) plus push/repository
		// webhooks. Every other tail -- the single-commit read
		// /commits/{sha} (a different response shape), /check-suites,
		// /pulls, /comments, ... -- stays passthrough, forwarded by the
		// dispatcher instead of falling to the router's NotFound.
		r.Get("/repos/{owner}/{repo}/commits/*", h.commitsSubtree)

		// The LEGACY statuses-list spelling /statuses/{ref} -- the path the
		// consumers actually send (required-builds paginates it per_page=100
		// until a short page) -- lands in the SAME handler and row space as
		// the modern /commits/{ref}/statuses form above. GET only: the
		// required-builds status PUBLISH (POST /statuses/{sha}) falls to
		// MethodNotAllowed and the passthrough proxy, untouched.
		r.Get("/repos/{owner}/{repo}/statuses/*", h.statusesAlias)

		// Cached compare (respcache_compare.go): the three-dot base...head
		// comparison pr-minder's auto_open_pr / close-empty gates run per
		// branch. Greedy wildcard because branch names carry slashes; the
		// files array's presence + per-file counts are preserved exactly
		// (the empty-PR gate), and query params / diff-patch Accepts /
		// cross-fork owner:branch baseheads pass through. Flushed by
		// push/repository webhooks.
		r.Get("/repos/{owner}/{repo}/compare/*", h.cachedCompare)

		// Cached workflow-runs listing (respcache_actionsruns.go): the
		// per-sha runs page pr-minder's zombie probe (reads total_count
		// only) and required-builds' listWorkflowRuns poll. head_sha is
		// REQUIRED for a cacheable shape (the unfiltered listing churns
		// constantly and is deliberately unmodeled); any other filter param
		// passes through, and deeper /actions/runs/{id}/... paths keep
		// falling to the NotFound passthrough (this is an exact-literal
		// registration). Flushed per sha by status/check_run/check_suite/
		// workflow_job events, repo-wide by repository, + 24h TTL (run
		// DELETION emits no webhook).
		r.Get("/repos/{owner}/{repo}/actions/runs", h.cachedWorkflowRuns)

		// Cached Actions JOB reads (respcache_workflowjobs.go): a run's jobs
		// page and a single job. Only TERMINAL answers are stored -- a
		// queued/in_progress job is a live value the runner coordinator
		// provisions against, so those always reach GitHub; what this kills
		// is the fleet re-reading SETTLED runs forever. A re-run replaces a
		// run's jobs under the same run id, so workflow_job/workflow_run
		// deliveries flush every row under that run.
		r.Get("/repos/{owner}/{repo}/actions/runs/{run_id}/jobs", h.cachedRunJobs)
		r.Get("/repos/{owner}/{repo}/actions/jobs/{job_id}", h.cachedWorkflowJob)

		// Cached Code Quality setup (respcache_codequality.go): the per-repo
		// enablement config, modeled from GitHub's OpenAPI `code-quality-setup`
		// schema. GitHub emits no webhook when it changes, so the PATCH is
		// registered too -- purely to flush the row before proxying the write,
		// which is the only change signal the mirror can see. A change made
		// outside the mirror is bounded by the (deliberately short) TTL.
		r.Get("/repos/{owner}/{repo}/code-quality/setup", h.cachedCodeQualitySetup)
		r.Patch("/repos/{owner}/{repo}/code-quality/setup", h.patchCodeQualitySetup)

		// Cached single-label read (respcache_labels.go): the label
		// DEFINITION pr-minder re-reads per repo per sweep. `label`
		// deliveries flush the repo's rows -- the grain is the repo because
		// a rename moves two names at once -- and the PATCH/DELETE on this
		// same path are registered purely to flush before proxying, so a
		// caller cannot read back its own stale write in the seconds before
		// the delivery lands. A label name carrying a slash matches no route
		// segment and keeps passing through; so does the labels LIST.
		r.Get("/repos/{owner}/{repo}/labels/{name}", h.cachedLabel)
		r.Patch("/repos/{owner}/{repo}/labels/{name}", h.writeLabel)
		r.Delete("/repos/{owner}/{repo}/labels/{name}", h.writeLabel)

		// Cached bare-repo read (respcache_repo.go): rebuilt from the repos
		// TRUTH row itself -- no snapshot table and no per-row TTL, mirroring
		// how tier 1 serves truth (repository webhooks, fleet sync, and the
		// consistency check keep the row converged; the reveal probe
		// re-absorbs it per principal within the grant TTL). Served only when
		// the row answers completely (known visibility -- unknown fails
		// closed -- default branch, full name); anything else fetches and
		// absorbs. Query params and non-default Accepts pass through, and
		// HEAD requests fall to MethodNotAllowed → the proxy.
		r.Get("/repos/{owner}/{repo}", h.cachedRepo)

		// Cached branches list (respcache_branches.go): per-page whole-doc
		// snapshots. Branch create/delete/tip-move all arrive as pushes, so
		// push/repository webhooks flush repo-wide. The single-branch read
		// /branches/{branch} is a different shape and stays passthrough.
		r.Get("/repos/{owner}/{repo}/branches", h.cachedBranchesList)

		// Cached installation-repositories listing
		// (respcache_installationrepos.go): "which repos does the token I am
		// holding cover". Keyed by the BEARER's fingerprint, not by the
		// requireAuth principal -- the app:<id> principal is shared across
		// every token of an app, including tokens of different installations,
		// which see different repositories.
		r.Get("/installation/repositories", h.cachedInstallationRepos)

		// Cached webhook CONFIGURATION listings (respcache_hooks.go), for a
		// repository and for an organization. Keyed by the BEARER's
		// fingerprint, and here that is the security boundary rather than a
		// convenience: these are ADMIN-only reads, while the reveal layer
		// proves READ access and admits any principal on a public repo -- so a
		// global row behind the ordinary gate would leak a repo's webhook
		// endpoints to a read-only caller. The write verbs on the same paths
		// are registered to flush before proxying, across every credential,
		// because one caller's hook change moves everyone's answer.
		r.Get("/repos/{owner}/{repo}/hooks", h.cachedRepoHooks)
		r.Post("/repos/{owner}/{repo}/hooks", h.writeRepoHooks)
		r.Patch("/repos/{owner}/{repo}/hooks/{hook_id}", h.writeRepoHooks)
		r.Delete("/repos/{owner}/{repo}/hooks/{hook_id}", h.writeRepoHooks)
		r.Get("/orgs/{org}/hooks", h.cachedOrgHooks)
		r.Post("/orgs/{org}/hooks", h.writeOrgHooks)
		r.Patch("/orgs/{org}/hooks/{hook_id}", h.writeOrgHooks)
		r.Delete("/orgs/{org}/hooks/{hook_id}", h.writeOrgHooks)

		// Cached PR routes (respcache_pulls.go + respcache_pullfiles.go): the
		// open-PR list is served from webhook-maintained pull_requests state
		// behind a per-repo "list complete" marker; the single PR is served
		// from an open row only when it is rest-complete AND its `mergeable`
		// is KNOWN — a null/unknown mergeable always misses so pr-minder's
		// resolve-poll still reaches GitHub — or, for a CLOSED PR, from its
		// rendered doc snapshot (diff/raw Accepts and unknown query shapes
		// pass through). The exact /pulls/{number}/files tail is cached as
		// per-page whole-doc snapshots, flushed per PR by pull_request events
		// and repo-wide by push/repository; every OTHER sub-resource
		// (/reviews, /merge, /commits, ...) matches none of these patterns
		// and keeps passing through, and writes (POST/PATCH/PUT) fall to
		// MethodNotAllowed → the proxy.
		r.Get("/repos/{owner}/{repo}/pulls", h.cachedPullsList)
		r.Get("/repos/{owner}/{repo}/pulls/{number}", h.cachedPull)
		r.Get("/repos/{owner}/{repo}/pulls/{number}/files", h.cachedPullFiles)

		// A PR's commits (respcache_pullcommits.go): GitHub answers the same
		// item shape as the repository commits list, so this reuses that
		// route's storage whole -- the commits land in the one global
		// git_commits_cache and the page's ordered shas in a
		// commits_list_cache snapshot under a synthetic "pull/<n>/commits"
		// ref key. Flushed per PR by pull_request events, repo-wide by push.
		r.Get("/repos/{owner}/{repo}/pulls/{number}/commits", h.cachedPullCommits)
	})

	// Fallback: any request the mirror does not specifically serve is forwarded
	// to GitHub, uncached, using the caller's own token. This makes the mirror a
	// drop-in for api.github.com — cached endpoints stay fast, and every other
	// endpoint still works. chi runs r.Use middleware (CORS, recoverer) around
	// these, so forwarded responses carry CORS headers and preflight is handled.
	// MethodNotAllowed covers a known path hit with an unregistered method; the
	// proxy itself enforces the bearer-token requirement.
	// Both fallbacks tag WHY they forwarded, so the dashboard separates "no
	// cached route claims this path" (every new caching candidate starts here)
	// from a cached route declining a shape it does model the path for.
	r.NotFound(taggedProxy(ghProxy, PassUnrouted))
	r.MethodNotAllowed(taggedProxy(ghProxy, PassMethod))

	return r
}
