package api

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/wow-look-at-my/github-state-mirror/internal/httpobs"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
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
// onResponse (nil-safe) observes every proxied upstream response after the
// rate meter; the router wires the auth-failure mint invalidation there.
//
// observe (nil-safe) charts the mirror→GitHub leg. It is installed on the
// TRANSPORT rather than in ModifyResponse for two reasons: a request that
// dies before a response (the ErrorHandler path) is still a real request and
// still belongs on the chart, and the transport is the one place every
// proxied byte must pass through. This is also the choke point for the
// DEBOUNCED reads, whose N inbound bars share exactly one call here -- the
// debouncer used to record that leg itself, which was one manual call site
// away from the batch path being the only observed one.
func newGitHubProxy(baseURL string, meter *ratemeter.Store, onResponse func(*http.Response), observe httpobs.Observer) http.Handler {
	target, err := url.Parse(baseURL)
	if err != nil {
		// baseURL is operator config, not caller input: panic here, not per-request.
		panic("api: invalid GitHub base URL " + baseURL + ": " + err.Error())
	}

	rp := &httputil.ReverseProxy{
		Transport: httpobs.Transport(nil, observe),
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Deliberately skips SetXForwarded: GitHub does not need the client's address.
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
		if bearerToken(r) == "" {
			http.Error(w, "unauthorized: missing Authorization header", http.StatusUnauthorized)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// stripUpstreamCORS removes GitHub's CORS headers so they never duplicate the
// mirror's own corsMiddleware output (browsers reject a doubled
// Access-Control-Allow-Origin). Access-Control-Expose-Headers stays, so
// cross-origin clients can still read GitHub's X-RateLimit-*, Link, and similar headers.
func stripUpstreamCORS(resp *http.Response) error {
	h := resp.Header
	h.Del("Access-Control-Allow-Origin")
	h.Del("Access-Control-Allow-Methods")
	h.Del("Access-Control-Allow-Headers")
	h.Del("Access-Control-Allow-Credentials")
	h.Del("Access-Control-Max-Age")
	return nil
}
