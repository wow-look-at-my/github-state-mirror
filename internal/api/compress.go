package api

import (
	"bytes"
	"compress/gzip"
	"log/slog"
	"net/http"
	"strings"
)

// Response compression for the DASHBOARD's own JSON/binary payloads.
//
// The origin is grey-clouded (Cloudflare proxying off, after throttling), so
// nothing in front of the app compresses any more: if a payload is to arrive
// compressed, this file is what compresses it. It is deliberately scoped to
// the dashboard's buffered admin endpoints and is NOT wired into the GitHub
// data plane — the cached-route rebuilds have a pinned response-header
// contract, the passthrough proxy relays GitHub's own encoding untouched, and
// the consistency check's NDJSON stream must keep flushing per line.
//
// gzip only, at BestSpeed. Every browser accepts gzip, so there is no
// negotiation to get wrong, and on the shape that matters — the columnar
// timeline payload — level 1 is 1006 KB in 24 ms against level 6's 952 KB in
// 140 ms: 6% more bytes for a sixth of the CPU (measured in
// prototype/timelinewire, TestStdlibGzip).

// compressMinBytes is the floor below which compressing costs more than it
// saves (a small payload can even grow past the gzip header/trailer).
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
		// Compression is an optimization; a failure must never cost the
		// operator the payload.
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

// mediaTypeOf strips any parameters (";q=0.9") from one Accept element and
// lowercases what is left.
func mediaTypeOf(part string) string {
	if i := strings.IndexByte(part, ';'); i >= 0 {
		part = part[:i]
	}
	return strings.TrimSpace(strings.ToLower(part))
}
