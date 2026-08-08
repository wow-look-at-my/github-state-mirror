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

// The two paths docker-updater discovers by itself, with no label to configure.
// RFC 8615 reserves /.well-known/ for exactly this: a path an automated client
// may request without prior arrangement.
const (
	wellKnownHealth    = "/.well-known/docker-updater/health"
	wellKnownPreUpdate = "/.well-known/docker-updater/pre-update"
)

// pingTimeout bounds the health probe's database check. SQLite on one local
// file answers in microseconds, so this only fires on a database that has
// genuinely stopped answering.
const pingTimeout = 2 * time.Second

// registerUpdateChecks wires both endpoints. They answer with a status code and
// nothing else; anything in the body is ignored by the prober.
func registerUpdateChecks(r chi.Router, store *ghdata.Store, mgr *freshness.Manager) {
	// health answers "this container is up and serving". It is polled after an
	// update until it passes or the budget expires, and a failure rolls the
	// update back -- so it asserts what a 200 alone cannot: that the cache
	// database behind the server still answers.
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

	// pre-update answers "may I be replaced right now". A detached fetch is the
	// one thing here an update destroys: it runs on a context deliberately
	// decoupled from any request so its results always land, and a replaced
	// container abandons it after the upstream calls have already been spent.
	// Holding costs one check cycle -- docker-updater retries -- so a busy
	// mirror updates between fetches instead of never.
	r.Get(wellKnownPreUpdate, func(w http.ResponseWriter, _ *http.Request) {
		if mgr.Busy() {
			slog.Info("update-check pre-update: holding, fetches in flight")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
