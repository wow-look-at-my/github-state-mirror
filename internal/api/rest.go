package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
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
	// gh supplies the GitHub base URL for the cached routes' upstream fetches and
	// VerifyAppIdentity for the token-mint route (respcache.go).
	gh *ghclient.Client
	// upstream is the HTTP client the cached routes fetch misses with. Distinct
	// from the passthrough proxy: a cached route must buffer and absorb the
	// response, not stream it.
	upstream *http.Client
	// meter passively records X-RateLimit-* headers off every upstream response
	// (miss fetches, reveal probes) for the dashboard's "Rate limit" tab.
	// Nil-safe: a nil meter records nothing.
	meter *ratemeter.Store
	// recordIdentity persists a principal->display-name mapping (the shared,
	// debounced identityRecorder requireAuth uses). The self-verifying app-JWT
	// routes (token mint, repo installation) call it so app:<id> principals
	// resolve to their slug on the dashboard too. Nil-safe: nil records nothing.
	recordIdentity identityRecorder
}

// NOTE: the mirror once served /user, /user/orgs, /compare, and /pulls/{n}/files
// from cache with ad-hoc trimmed shapes and no contract; those routes were
// removed and now fall through to the verbatim passthrough proxy (router.go).
// Today's cached REST routes (respcache.go) are deliberately trimmed again —
// but under an explicit contract: absorb the response's state, rebuild it
// without any URL field (url, *_url, _links), same shape on hit and miss.
// Byte-identity applies only to the GraphQL org-repos route (identity test in
// identity_shape_test.go).

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("json encode failed", "error", err)
	}
}
