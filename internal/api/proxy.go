package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghjson"
	"github.com/wow-look-at-my/github-state-mirror/internal/ratemeter"
)

// newGitHubProxy returns an http.Handler that reverse-proxies a request to
// GitHub (rooted at baseURL, normally https://api.github.com), strips URL/link
// noise from JSON responses, and otherwise leaves the response uncached.
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
func newGitHubProxy(baseURL string, meter *ratemeter.Store, onResponse func(*http.Response)) http.Handler {
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
		ModifyResponse: func(resp *http.Response) error {
			// Passively record the X-RateLimit-* headers GitHub attached; the
			// passthrough proxy is the highest-volume upstream path, so it is
			// the rate meter's main feed. resp.Request is the outbound clone,
			// which carries the inbound request's context and headers, so
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

// stripUpstreamCORS removes duplicate CORS headers, URL-bearing Link headers,
// and URL/link fields inside JSON response bodies.
func stripUpstreamCORS(resp *http.Response) error {
	h := resp.Header
	h.Del("Access-Control-Allow-Origin")
	h.Del("Access-Control-Allow-Methods")
	h.Del("Access-Control-Allow-Headers")
	h.Del("Access-Control-Allow-Credentials")
	h.Del("Access-Control-Max-Age")
	h.Del("Link")

	if !isJSONResponse(resp) {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	stripped, err := ghjson.StripURLFields(body)
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		h.Set("Content-Length", strconv.Itoa(len(body)))
		return nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(stripped))
	resp.ContentLength = int64(len(stripped))
	h.Set("Content-Length", strconv.Itoa(len(stripped)))
	h.Del("Etag")
	h.Del("Last-Modified")
	return nil
}

func isJSONResponse(resp *http.Response) bool {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.Contains(ct, "application/json") || strings.Contains(ct, "+json")
}
