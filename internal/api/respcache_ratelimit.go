package api

import (
	"log/slog"
	"net/http"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// Implements GET /rate_limit: rebuilds GitHub's own `resources` shape from
// ratemeter's passive X-RateLimit-* observations instead of ever asking
// GitHub again. See docs/cache/uncacheable-routes.md for why this is not a
// TTL-bounded store row.
func (h *handlers) cachedRateLimit(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || !acceptsDefaultJSON(r) {
		// No query parameter is documented; an unmodeled shape falls through.
		h.passthrough(w, r, shapeReason(r, true))
		return
	}

	identity := actor.FromContext(r.Context())
	if identity == "" {
		// requireAuth always sets; this guards a route wired without it.
		identity = callerLabel(r).Key
	}

	resp := ghclient.RateLimitResponse{Resources: map[string]ghclient.RateLimitResource{}}
	for _, o := range h.meter.ObservationsFor(identity) {
		resp.Resources[o.Resource] = ghclient.RateLimitResource{
			Limit: o.Limit, Remaining: o.Remaining, Used: o.Used, Reset: o.Reset,
		}
	}
	// GitHub also sends core as the deprecated top-level `rate`; send both so no caller field goes empty.
	if core, ok := resp.Resources["core"]; ok {
		resp.Rate = &core
	}

	body, err := marshalTrimmed(resp)
	if err != nil {
		slog.Warn("rate limit render failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.reqlog.observe(r, DispHit)
	writeRebuilt(w, http.StatusOK, body, true)
}
