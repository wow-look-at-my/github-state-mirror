package api

import (
	"net/http"
)

// corsMiddleware adds CORS headers so browser apps on other origins (e.g. the
// repo-nightmare PR viewer served from GitHub Pages) can call the mirror's API.
//
// The mirror's security boundary is the per-token fingerprint, not the request
// origin, so a permissive default is safe: a cross-origin caller still must
// present a valid GitHub token, which a browser cannot read from another origin.
//
// allowed is the set of permitted origins. When it is empty or contains "*",
// any origin is allowed (Access-Control-Allow-Origin: *). Otherwise only an
// exact-match Origin is echoed back; a non-matching Origin gets no ACAO header
// and the browser blocks the response.
//
// Preflight OPTIONS requests are answered here with 204 and never reach
// requireAuth, because browsers do not send the Authorization header on
// preflight. chi runs r.Use middleware before route matching, so this also
// intercepts preflight for method-specific routes like POST /graphql.
func corsMiddleware(allowed []string) func(http.Handler) http.Handler {
	wildcard := len(allowed) == 0
	allowSet := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		if o == "*" {
			wildcard = true
		}
		allowSet[o] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if wildcard {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if origin != "" {
				if _, ok := allowSet[origin]; ok {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Add("Vary", "Origin")
				}
			}

			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
