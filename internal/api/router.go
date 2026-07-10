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
) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	// CORS so browser clients on other origins (e.g. the repo-nightmare PR
	// viewer on GitHub Pages) can call the API. Mounted before the auth group
	// so preflight OPTIONS is answered without a token.
	r.Use(corsMiddleware(allowedOrigins))

	// In-memory record of data-API requests (hit/miss/passthrough) for the
	// dashboard's "Requests" view.
	reqlog := newRequestLog()

	// Transparent GitHub passthrough for anything the mirror does not serve
	// itself. Built from the same base URL the cache fetchers use, so forwarded
	// requests reach the same upstream (a fake server in tests). Wrapped so every
	// proxied request is recorded as a passthrough.
	ghProxy := recordPassthrough(newGitHubProxy(gh.BaseURL(), meter), reqlog)

	h := &handlers{mgr: mgr, store: store, ghProxy: ghProxy, reqlog: reqlog, gh: gh, upstream: &http.Client{}, meter: meter}

	// Web dashboard: static page, GitHub OAuth login, and the cache-stats API.
	// Authorized by session cookie (login), distinct from the data API below.
	newDashboard(authSvc, store, baseURL, reqlog, checker, meter, notifier).routes(r)

	// Webhook endpoint — authenticated by HMAC signature (X-Hub-Signature-256),
	// not a user token, so it sits outside the requireAuth group. After each
	// synchronous dispatch the subscriber notifier fans the outcome out to
	// registered endpoints (non-blocking; nil keeps the feature inert).
	r.Post("/webhook", webhook.Handler(webhookSecret, dispatcher, notifier))

	// GitHub OAuth token-exchange relay for browser clients. A purely
	// client-side app cannot POST to github.com/login/oauth/access_token
	// directly (that endpoint sends no CORS headers); the mirror relays it with
	// correct CORS. It carries no bearer token (the OAuth client secret in the
	// body is the credential), so it sits outside requireAuth, and it targets
	// github.com — not the api.github.com passthrough.
	r.Post("/login/oauth/access_token", h.oauthAccessToken)

	// Installation-token mint cache and the repo-installation lookup.
	// Registered OUTSIDE requireAuth: the bearer token on both is a GitHub App
	// JWT (it cannot resolve GET /user), so each handler verifies it itself
	// via VerifyAppIdentity and partitions by the verified app id.
	// Unverifiable callers are forwarded unchanged.
	r.Post("/app/installations/{id}/access_tokens", h.cachedInstallationToken)
	r.Get("/repos/{owner}/{repo}/installation", h.cachedRepoInstallation)

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
		r.Use(requireAuth(gh, newIdentityRecorder(store)))

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

		// Cached commits LIST (respcache_commits.go): per-page sha snapshots
		// over the same git_commits_cache rows, flushed by push/repository
		// webhooks.
		r.Get("/repos/{owner}/{repo}/commits", h.cachedCommitsList)

		// The /commits/* subtree dispatcher (respcache_commitci.go): a tail
		// ending in /status (the combined commit status) or /check-runs is a
		// cached commit-CI route -- the suffix anchor is what lets the ref
		// carry slashes, which a single-segment {ref} parameter could never
		// match. Snapshots are keyed by the VERBATIM ref (branch, sha, or
		// tag; never resolved) and flushed by status/check_run/check_suite
		// (CI moved) plus push/repository webhooks. Every other tail -- the
		// single-commit read /commits/{sha} (a different response shape),
		// the raw /statuses list, /check-suites, /pulls, /comments, ... --
		// stays passthrough, now forwarded by the dispatcher instead of
		// falling to the router's NotFound.
		r.Get("/repos/{owner}/{repo}/commits/*", h.commitsSubtree)

		// Cached compare (respcache_compare.go): the three-dot base...head
		// comparison pr-minder's auto_open_pr / close-empty gates run per
		// branch. Greedy wildcard because branch names carry slashes; the
		// files array's presence + per-file counts are preserved exactly
		// (the empty-PR gate), and query params / diff-patch Accepts /
		// cross-fork owner:branch baseheads pass through. Flushed by
		// push/repository webhooks.
		r.Get("/repos/{owner}/{repo}/compare/*", h.cachedCompare)

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
	})

	// Fallback: any request the mirror does not specifically serve is forwarded
	// to GitHub, uncached, using the caller's own token. This makes the mirror a
	// drop-in for api.github.com — cached endpoints stay fast, and every other
	// endpoint still works. chi runs r.Use middleware (CORS, recoverer) around
	// these, so forwarded responses carry CORS headers and preflight is handled.
	// MethodNotAllowed covers a known path hit with an unregistered method; the
	// proxy itself enforces the bearer-token requirement.
	r.NotFound(ghProxy.ServeHTTP)
	r.MethodNotAllowed(ghProxy.ServeHTTP)

	return r
}
