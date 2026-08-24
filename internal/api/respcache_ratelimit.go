package api

import (
	"log/slog"
	"net/http"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// This file implements GET /rate_limit.
//
// GitHub's own answer to this endpoint does not count against the caller's
// quota, but polling it still costs a full round trip -- and, before this
// route existed, the passthrough debouncer's hold too (PASSTHROUGH_DEBOUNCE,
// default 5s -- debounce.go), which is backwards for an endpoint whose whole
// point is telling a caller its standing QUICKLY. The mirror already learns
// every credential's standing for free off every response it makes on that
// credential's behalf -- every cached-route miss, the reveal probe, the
// passthrough proxy itself (ratemeter.Store, fed from internal/api's
// fetchUpstream and newGitHubProxy) -- so this route rebuilds GitHub's own
// `resources` shape from that memory instead of ever asking GitHub again.
// Registered as a real, served route (not left to the NotFound passthrough)
// purely for backward compatibility with callers that still poll GET
// /rate_limit directly: their answer is now free and instant instead of a
// real GitHub round trip that also paid the debounce hold.
//
// Inside requireAuth like every other data route, so the identity keying this
// serves from ("user:<id>" for a resolved user token, "app:<id>" for a
// verified X-Mirror-Identity caller, a token fingerprint otherwise) is
// EXACTLY the identity h.fetchUpstream's meter.Observe call already
// accumulates that same principal's cached-route traffic under -- an
// unauthenticated or per-raw-token keying here would answer from a
// near-always-empty bucket, since almost all of a principal's GitHub calls
// are cached-route misses recorded under its resolved actor, not its bare
// token.
//
// Staleness: a resource with no live observation -- never seen, or its
// window already rolled over -- is simply OMITTED, per
// ratemeter.Store.ObservationsFor's own window-aware dead() rule, never a
// flat serve TTL. A flat TTL would blank out a caller's perfectly valid
// standing just because it did not happen to poll again within it; the
// meter's own rule is the honest bound instead.
func (h *handlers) cachedRateLimit(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || !acceptsDefaultJSON(r) {
		// GET /rate_limit takes no query parameters and GitHub always answers
		// JSON; an unmodeled shape (none is documented) falls through rather
		// than risk misinterpreting it.
		h.passthrough(w, r, shapeReason(r, true))
		return
	}

	identity := actor.FromContext(r.Context())
	if identity == "" {
		// requireAuth always sets one; this only guards a route wired without
		// it (a test, or a future refactor), matching the fallback style
		// elsewhere in this package (see callerLabel).
		identity = callerLabel(r).Key
	}

	resp := ghclient.RateLimitResponse{Resources: map[string]ghclient.RateLimitResource{}}
	for _, o := range h.meter.ObservationsFor(identity) {
		resp.Resources[o.Resource] = ghclient.RateLimitResource{
			Limit: o.Limit, Remaining: o.Remaining, Used: o.Used, Reset: o.Reset,
		}
	}
	// GitHub sends core twice: once in resources, once as the deprecated
	// top-level `rate`. Send both, so pointing an existing caller at the mirror
	// cannot quietly empty a field it reads.
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
