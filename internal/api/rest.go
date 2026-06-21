package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

type handlers struct {
	mgr   *freshness.Manager
	store *ghdata.Store
	// ghProxy forwards requests the mirror does not serve from cache straight to
	// GitHub, uncached (recording each as a passthrough). The GraphQL handler uses
	// it for queries it cannot answer from the cache; it is also the router's
	// NotFound/MethodNotAllowed fallback.
	ghProxy http.Handler
	// reqlog records per-request cache dispositions (hit/miss/passthrough) for the
	// dashboard's "Requests" view.
	reqlog *requestLog
}

// NOTE: the mirror used to serve /user, /user/orgs, /compare, and
// /pulls/{n}/files from cache, but with TRIMMED response shapes (a subset of
// GitHub's fields). Those reconstructed caches remain removed; unsupported REST
// routes fall through to the passthrough proxy, which strips URL/link noise from
// JSON but does not populate the cache.

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("json encode failed", "error", err)
	}
}
