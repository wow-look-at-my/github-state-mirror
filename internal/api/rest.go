package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
	"github.com/wow-look-at-my/github-state-mirror/internal/reqtimeline"
)

type handlers struct {
	mgr   *freshness.Manager
	store *ghdata.Store
	// ghProxy is the passthrough fallback for anything not served from cache.
	ghProxy http.Handler
	// reqlog records per-request cache dispositions for the Requests tab.
	reqlog *requestLog
	// gh supplies upstream fetches' base URL and VerifyAppIdentity.
	gh *ghclient.Client
	// upstream buffers and absorbs a miss; the passthrough proxy streams instead.
	upstream *http.Client
	// meter records X-RateLimit-* for the Rate limit tab. Nil-safe.
	meter *ratemeter.Store
	// recordIdentity maps a principal to a display name. Nil-safe.
	recordIdentity identityRecorder
	// timeline records each miss's timed fetch for the Timeline chart. Nil-safe.
	timeline *reqtimeline.Recorder
	// shapes captures uncached traffic's key/type shape for /api/brief. Nil-safe.
	shapes *shapeStore
}

// Cached REST routes rebuild the response without any URL field, under an
// explicit contract. see docs/cache/three-tier-contract.md

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("json encode failed", "error", err)
	}
}
