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
