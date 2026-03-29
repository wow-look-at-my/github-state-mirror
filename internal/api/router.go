package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

func NewRouter(
	mgr *freshness.Manager,
	store *ghdata.Store,
	webhookSecret string,
	dispatcher *syncpkg.WebhookDispatcher,
) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

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
