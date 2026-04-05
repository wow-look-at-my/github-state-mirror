package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// passthroughAuth extracts the caller's Authorization header, injects the
// token into the request context, resolves the GitHub login, and sets
// the actor so all cache operations are scoped per-user.
func passthroughAuth(gh *ghclient.Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if auth := r.Header.Get("Authorization"); auth != "" {
				token := strings.TrimPrefix(auth, "Bearer ")
				token = strings.TrimPrefix(token, "token ")
				if token != "" {
					ctx := ghclient.WithToken(r.Context(), token)
					login, err := gh.ResolveActor(ctx)
					if err != nil {
						slog.Warn("resolve actor failed", "error", err)
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					ctx = actor.WithActor(ctx, login)
					r = r.WithContext(ctx)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func NewRouter(
	mgr *freshness.Manager,
	store *ghdata.Store,
	webhookSecret string,
	dispatcher *syncpkg.WebhookDispatcher,
	gh *ghclient.Client,
) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(passthroughAuth(gh))

	h := &handlers{mgr: mgr, store: store}

	// REST endpoints
	r.Get("/user", h.getUser)
	r.Get("/user/orgs", h.getUserOrgs)
	r.Get("/repos/{owner}/{repo}/compare/{base}...{head}", h.getCompare)
	r.Get("/repos/{owner}/{repo}/pulls/{number}/files", h.getPRFiles)

	// GraphQL endpoint
	r.Post("/graphql", h.graphql)

	// Webhook endpoint
	r.Post("/webhook", webhook.Handler(webhookSecret, dispatcher))

	return r
}
