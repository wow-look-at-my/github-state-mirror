package api

import (
	"bytes"
	"compress/gzip"
	"log/slog"
	"net/http"
	"strings"
)

// Gzip-compresses the dashboard's own buffered admin payloads; the GitHub
// data plane keeps its own pinned header contract untouched.
// see docs/timeline-wire-format.md

// compressMinBytes is the floor below which gzip can grow the payload.
const compressMinBytes = 1400

// writeBody writes a fully-buffered response body, gzipped when the caller
// accepts gzip and the payload is big enough to be worth it. Vary is always
// set: the same URL legitimately answers with different encodings, and a
// shared cache that misses that would serve gzip bytes to a client that never
// asked for them.
func writeBody(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	addVary(h, "Accept-Encoding")

	if len(body) < compressMinBytes || !acceptsGzip(r) {
		_, _ = w.Write(body)
		return
	}
	var out bytes.Buffer
	out.Grow(len(body) / 4)
	zw, err := gzip.NewWriterLevel(&out, gzip.BestSpeed)
	if err == nil {
		if _, err = zw.Write(body); err == nil {
			err = zw.Close()
		}
	}
	if err != nil {
		// A compression failure must never cost the operator the payload.
		slog.Warn("response gzip failed", "path", r.URL.Path, "error", err)
		_, _ = w.Write(body)
		return
	}
	h.Set("Content-Encoding", "gzip")
	_, _ = w.Write(out.Bytes())
}

// acceptsGzip reports whether the client accepts gzip, honoring an explicit
// `gzip;q=0` refusal.
func acceptsGzip(r *http.Request) bool {
	for _, part := range splitList(r.Header.Get("Accept-Encoding")) {
		name, params, _ := strings.Cut(part, ";")
		if strings.TrimSpace(strings.ToLower(name)) != "gzip" {
			continue
		}
		return !strings.Contains(strings.ReplaceAll(strings.ToLower(params), " ", ""), "q=0")
	}
	return false
}

// addVary appends a field name to Vary without duplicating it.
func addVary(h http.Header, field string) {
	for _, existing := range splitList(h.Get("Vary")) {
		if strings.EqualFold(strings.TrimSpace(existing), field) {
			return
		}
	}
	h.Add("Vary", field)
}

// splitList splits a comma-separated header value into trimmed elements,
// skipping empties.
func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
