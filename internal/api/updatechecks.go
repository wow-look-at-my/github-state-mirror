package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// docker-updater discovers these by RFC convention, with no label needed.
const (
	wellKnownHealth    = "/.well-known/docker-updater/health"
	wellKnownPreUpdate = "/.well-known/docker-updater/pre-update"
)

// pingTimeout only fires on a database that has genuinely stopped answering.
const pingTimeout = 2 * time.Second

// registerUpdateChecks wires both endpoints. They answer with a status code and
// nothing else; anything in the body is ignored by the prober.
func registerUpdateChecks(r chi.Router, store *ghdata.Store, mgr *freshness.Manager) {
	// Asserts what a bare cannot: that the cache database still answers.
	r.Get(wellKnownHealth, func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), pingTimeout)
		defer cancel()
		if err := store.Ping(ctx); err != nil {
			slog.Error("update-check health: database unreachable", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// Holds while a detached fetch is in flight; docker-updater retries, so a
	// busy mirror updates between fetches instead of never.
	r.Get(wellKnownPreUpdate, func(w http.ResponseWriter, _ *http.Request) {
		if mgr.Busy() {
			slog.Info("update-check pre-update: holding, fetches in flight")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
