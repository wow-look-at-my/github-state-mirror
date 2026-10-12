package api

import (
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// A sub-resource refusal is per credential. The reveal gate proves the caller
// can read the REPO. GitHub can still refuse one of its sub-resources to that
// credential, for a permission the token lacks. That answer is stored in the
// deny cache under the caller's own principal, so it is never served to
// another credential.

// subresourceVerdict returns the caller's stored refusal of a sub-resource.
func (h *handlers) subresourceVerdict(r *http.Request, kind, key string) (ghdata.DenyVerdict, bool) {
	principal := actor.FromContext(r.Context())
	if principal == "" {
		return ghdata.DenyVerdict{}, false
	}
	v, ok, err := h.store.GetDenyVerdict(r.Context(), principal, kind, key, time.Now())
	if err != nil {
		slog.Warn("sub-resource verdict read failed", "kind", kind, "key", key, "error", err, principalNameAttr(r.Context()))
		return ghdata.DenyVerdict{}, false
	}
	return v, ok
}

// recordSubresourceVerdict stores an authoritative refusal for the caller and
// reports whether resp was one. A counts unless it is rate limiting; statuses
// names any other status the route treats as a refusal.
func (h *handlers) recordSubresourceVerdict(r *http.Request, owner, repo, kind, key string, resp *http.Response, body []byte, statuses ...int) (ghdata.DenyVerdict, bool) {
	refusal := resp.StatusCode == http.StatusForbidden && !upstreamRateLimited(resp)
	if !refusal && !slices.Contains(statuses, resp.StatusCode) {
		return ghdata.DenyVerdict{}, false
	}
	v := ghdata.DenyVerdict{Status: resp.StatusCode, Message: upstreamErrorMessage(body)}
	principal := actor.FromContext(r.Context())
	if principal == "" {
		return v, true
	}
	if err := h.store.RecordDenyVerdict(r.Context(), principal, kind, key, owner, repo, v.Status, v.Message, time.Now()); err != nil {
		slog.Warn("sub-resource verdict write failed", "kind", kind, "key", key, "error", err, principalNameAttr(r.Context()))
	}
	return v, true
}
