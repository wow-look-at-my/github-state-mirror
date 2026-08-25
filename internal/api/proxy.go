package api

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

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
