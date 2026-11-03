// A sub-resource refusal is per credential.

package api

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// A sub-resource refusal is per credential. The reveal gate proves the caller can read the REPO.

// tokenRefusalPrefix starts GitHub's refusal of a token that lacks a permission.
const tokenRefusalPrefix = "Resource not accessible by "

// verdictKey is the deny-cache key of a refusal. A token refusal is keyed to the token's fingerprint. Another token of
// the same user may hold the permission, so the user principal must not carry the refusal.
func verdictKey(r *http.Request, principal, message string) string {
	if strings.HasPrefix(message, tokenRefusalPrefix) {
		if tok := bearerToken(r); tok != "" {
			return ghclient.Fingerprint(tok)
		}
	}
	return principal
}

// subresourceVerdict returns the caller's stored refusal of a sub-resource: the principal's, or the token's own.
func (h *handlers) subresourceVerdict(r *http.Request, kind, key string) (ghdata.DenyVerdict, bool) {
	principal := actor.FromContext(r.Context())
	if principal == "" {
		return ghdata.DenyVerdict{}, false
	}
	return h.storedVerdict(r, principal, kind, key)
}

// storedVerdict reads the refusal stored under the principal, then the one stored under the caller's token.
func (h *handlers) storedVerdict(r *http.Request, principal, kind, key string) (ghdata.DenyVerdict, bool) {
	keys := []string{principal}
	if tok := bearerToken(r); tok != "" {
		if fp := ghclient.Fingerprint(tok); fp != principal {
			keys = append(keys, fp)
		}
	}
	for _, k := range keys {
		v, ok, err := h.store.GetDenyVerdict(r.Context(), k, kind, key, time.Now())
		if err != nil {
			slog.Warn("sub-resource verdict read failed", "kind", kind, "key", key, "error", err, principalNameAttr(r.Context()))
			return ghdata.DenyVerdict{}, false
		}
		if ok {
			return v, true
		}
	}
	return ghdata.DenyVerdict{}, false
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
	if err := h.store.RecordDenyVerdict(r.Context(), verdictKey(r, principal, v.Message), kind, key, owner, repo, v.Status, v.Message, time.Now()); err != nil {
		slog.Warn("sub-resource verdict write failed", "kind", kind, "key", key, "error", err, principalNameAttr(r.Context()))
	}
	return v, true
}
