package api

import (
	"context"
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
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// requireAuth enforces that every data request carries a usable GitHub token.
// It validates the token against GitHub (rejecting absent, malformed, or
// revoked credentials with 401), injects the token into the request context,
// and scopes all cache operations to a fingerprint of that token.
//
// The cache partition (actor) is derived from the token itself, NOT the GitHub
// login, so that each credential only ever reads data it fetched. Two tokens
// belonging to the same user — e.g. a full-scope PAT and a read-only token
// granted to a third-party app — get separate buckets and can never observe
// each other's cached private data. Requests must never fall through to the
// service's own credentials (the GitHub App used for background refreshes),
// which may have far broader access than the caller.
func requireAuth(gh *ghclient.Client, record identityRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				http.Error(w, "unauthorized: missing Authorization header", http.StatusUnauthorized)
				return
			}
			ctx := ghclient.WithToken(r.Context(), token)
			// Validate the credential with GitHub up front (and warm the
			// token->login cache). The login does NOT become the bucket key —
			// the fingerprint does — but we remember the fingerprint->login
			// mapping so the dashboard can group a user's scopes by login.
			login, err := gh.ResolveActor(ctx)
			if err != nil {
				slog.Warn("resolve actor failed", "error", err)
				http.Error(w, "unauthorized: could not validate GitHub credential", http.StatusUnauthorized)
				return
			}
			fp := ghclient.Fingerprint(token)
			ctx = actor.WithActor(ctx, fp)
			record(ctx, fp, login)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// identityRecorder persists a fingerprint->login mapping for the dashboard.
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
) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	// CORS so browser clients on other origins (e.g. the repo-nightmare PR
	// viewer on GitHub Pages) can call the API. Mounted before the auth group
	// so preflight OPTIONS is answered without a token.
	r.Use(corsMiddleware(allowedOrigins))

	// Transparent GitHub passthrough for anything the mirror does not serve
	// itself. Built from the same base URL the cache fetchers use, so forwarded
	// requests reach the same upstream (a fake server in tests).
	ghProxy := newGitHubProxy(gh.BaseURL())

	h := &handlers{mgr: mgr, store: store, ghProxy: ghProxy}

	// Web dashboard: static page, GitHub OAuth login, and the cache-stats API.
	// Authorized by session cookie (login), distinct from the data API below.
	newDashboard(authSvc, store, baseURL).routes(r)

	// Webhook endpoint — authenticated by HMAC signature (X-Hub-Signature-256),
	// not a user token, so it sits outside the requireAuth group.
	r.Post("/webhook", webhook.Handler(webhookSecret, dispatcher))

	// Data endpoints — every request must carry a valid GitHub token, and all
	// cache access is scoped to that credential's fingerprint.
	r.Group(func(r chi.Router) {
		r.Use(requireAuth(gh, newIdentityRecorder(store)))

		// REST endpoints
		r.Get("/user", h.getUser)
		r.Get("/user/orgs", h.getUserOrgs)
		r.Get("/repos/{owner}/{repo}/compare/{base}...{head}", h.getCompare)
		r.Get("/repos/{owner}/{repo}/pulls/{number}/files", h.getPRFiles)

		// GraphQL endpoint
		r.Post("/graphql", h.graphql)
	})

	// Fallback: any request the mirror does not specifically serve is forwarded
	// to GitHub, uncached, using the caller's own token. This makes the mirror a
	// drop-in for api.github.com — cached endpoints stay fast, and every other
	// endpoint still works. chi runs r.Use middleware (CORS, recoverer) around
	// these, so forwarded responses carry CORS headers and preflight is handled.
	// MethodNotAllowed covers a known path hit with an unregistered method
	// (e.g. POST /user); the proxy itself enforces the bearer-token requirement.
	r.NotFound(ghProxy.ServeHTTP)
	r.MethodNotAllowed(ghProxy.ServeHTTP)

	return r
}
