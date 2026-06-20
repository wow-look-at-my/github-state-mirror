package api

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// hopByHop are connection-specific response headers that must not be copied when
// proxying, plus Content-Length (Go sets it from the body we write).
var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"content-length":      true,
}

// passthrough forwards a request the mirror does not cache straight to the
// upstream GitHub API and copies the response back verbatim. This is what makes
// the mirror a drop-in base URL for the GitHub API: a caller points at the
// mirror for everything, cached endpoints are served from SQLite, and anything
// else (single-PR reads, branches, reviews, every write, etc.) is transparently
// proxied. Each uncached path is logged at warn level so coverage gaps are
// visible and can be promoted to cached endpoints over time.
func (h *handlers) passthrough(w http.ResponseWriter, r *http.Request) {
	slog.Warn("uncached passthrough to upstream", "method", r.Method, "path", r.URL.Path)

	resp, err := h.gh.Forward(r.Context(), r.Method, r.URL.Path, r.URL.RawQuery, r.Header, r.Body)
	if err != nil {
		slog.Warn("passthrough upstream request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Warn("passthrough copy failed", "method", r.Method, "path", r.URL.Path, "error", err)
	}
}
