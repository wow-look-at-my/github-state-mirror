package api

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/wow-look-at-my/github-state-mirror/internal/httpobs"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
)

// newGitHubProxy reverse-proxies uncached requests to GitHub verbatim,
// rejecting a request with no Authorization header.
func newGitHubProxy(baseURL string, meter *ratemeter.Store, onResponse func(*http.Response), observe httpobs.Observer) http.Handler {
	target, err := url.Parse(baseURL)
	if err != nil {
		// baseURL is operator config, not caller input: panic here, not per-request.
		panic("api: invalid GitHub base URL " + baseURL + ": " + err.Error())
	}

	rp := &httputil.ReverseProxy{
		Transport: httpobs.Transport(nil, observe),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
		},
		ModifyResponse: func(resp *http.Response) error {
			// resp.Request is the outbound clone carrying the inbound context, so
			// callerLabel resolves the same identity the request log records.
			if resp.Request != nil {
				who := callerLabel(resp.Request)
				meter.Observe(who.Key, who.Name, resp)
			}
			if onResponse != nil {
				onResponse(resp)
			}
			return stripUpstreamCORS(resp)
		},
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

// stripUpstreamCORS drops GitHub's CORS headers so they never duplicate ours.
func stripUpstreamCORS(resp *http.Response) error {
	h := resp.Header
	h.Del("Access-Control-Allow-Origin")
	h.Del("Access-Control-Allow-Methods")
	h.Del("Access-Control-Allow-Headers")
	h.Del("Access-Control-Allow-Credentials")
	h.Del("Access-Control-Max-Age")
	return nil
}
