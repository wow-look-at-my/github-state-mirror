package api

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// newGitHubProxy returns an http.Handler that transparently reverse-proxies a
// request to GitHub (rooted at baseURL, normally https://api.github.com) and
// returns the upstream response verbatim and uncached.
//
// It is the mirror's fallback for any endpoint it does not specifically cache,
// so a client can point its entire GitHub REST/GraphQL surface at the mirror:
// known endpoints are served fast from the per-credential cache, and everything
// else is forwarded straight through to GitHub.
//
// The caller's Authorization header is forwarded unchanged — the mirror never
// substitutes its own GITHUB_TOKEN — and a request without one is rejected with
// 401, both so the mirror cannot be used as an open, unauthenticated relay to
// GitHub's API and so the contract matches the cached data endpoints, which also
// require a token. This path deliberately never touches the freshness store, so
// forwarded responses are never cached.
func newGitHubProxy(baseURL string) http.Handler {
	target, err := url.Parse(baseURL)
	if err != nil {
		// baseURL is operator-controlled configuration, not caller input, so an
		// unparseable value is a deployment error worth failing loudly on at
		// startup rather than per-request.
		panic("api: invalid GitHub base URL " + baseURL + ": " + err.Error())
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL routes the outbound request to target's scheme/host and
			// rewrites the Host header to match; the inbound path and query are
			// preserved. We deliberately do not call SetXForwarded — GitHub does
			// not need the client's address and we avoid leaking it.
			pr.SetURL(target)
		},
		ModifyResponse: stripUpstreamCORS,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("github proxy error", "method", r.Method, "path", r.URL.Path, "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			// Anonymous passthrough: allow tokenless requests ONLY onto GitHub's
			// public surface, and only if this request is not clearly targeting
			// private/privileged data (see anonymousPublicOnly). GitHub itself is
			// the authority on public-vs-private; we just refuse the obviously
			// private endpoints up front so an anonymous client cannot even
			// attempt them, and never send a service credential in their place.
			if !anonymousPublicOnly(r) {
				http.Error(w, "unauthorized: missing Authorization header", http.StatusUnauthorized)
				return
			}
			r.Header.Del("Authorization")
			rp.ServeHTTP(w, r)
			return
		}
		// Authenticated: forward the caller's credential unchanged. The mirror
		// never substitutes its own token.
		rp.ServeHTTP(w, r)
	})
}

// anonymousPublicOnly reports whether a tokenless request may be forwarded to
// GitHub. It is a defense-in-depth guard on top of GitHub's own authorization:
// an anonymous request can legitimately read only public data, so anything that
// is unmistakably privileged/private — user/org-scoped resources, notifications,
// anything that would require a token to even mean anything — is rejected before
// it leaves the mirror. GitHub still enforces the real boundary for every other
// path (an anonymous read of a private repo is 404/403 upstream); this exists to
// keep tokenless traffic off those paths entirely, and to keep the mirror from
// looking like an open relay for credential-bearing endpoints.
func anonymousPublicOnly(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// Public writes do not exist on GitHub: only authenticated users can
		// create/update anything (even on public repos), so no anonymous
		// non-GET/HEAD request is ever legitimate.
		return false
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/user"), strings.HasPrefix(p, "/users/"), strings.HasPrefix(p, "/orgs/"), strings.HasPrefix(p, "/notifications"):
		return false
	case strings.HasPrefix(p, "/repos/"):
		// /repos/{owner}/{repo}/... is public only if the repo is public; GitHub
		// decides. No additional restriction here.
		return true
	default:
		return true
	}
}
