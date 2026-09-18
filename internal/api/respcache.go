package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file holds the machinery every cached REST route shares — the upstream
// fetch, the verbatim replay, the rebuild writer — plus the index of the
// routes themselves. Each route family lives in its own respcache_*.go. The
// contract (see CLAUDE.md,
// "cache contract"): the mirror ABSORBS the state contained in a GitHub
// response into structured tables (internal/ghdata/respcache.go) and REBUILDS

const (
	// contentsCacheTTL is the TTL backstop on cached contents rows. Webhooks
	contentsCacheTTL = 24 * time.Hour

	// mintExpiryBuffer is subtracted from a minted token's expires_at to get
	mintExpiryBuffer = 10 * time.Minute

	// maxAbsorbBodyBytes caps how much of an upstream response the cached
	maxAbsorbBodyBytes = 8 << 20 //  MiB

	// maxMintBodyBytes caps the buffered token-mint request body (a
	maxMintBodyBytes = 1 << 20 //  MiB

	// cacheHeader marks responses served by a cached route: "hit" (rebuilt
	cacheHeader = "X-GSM-Cache"
)

// refreshGrantOn2xx renews the caller's grant after a successful repo-scoped
// fetch with their own token: GitHub just re-proved their access. Best-effort.
func (h *handlers) refreshGrantOn2xx(r *http.Request, owner, repo string, status int) {
	if status < 200 || status >= 300 {
		return
	}
	principal := actor.FromContext(r.Context())
	if principal == "" {
		return
	}
	if err := h.store.RecordGrant(r.Context(), principal, owner, repo, ghdata.GrantSourceProbe, time.Now()); err != nil {
		slog.Warn("refresh grant failed", "principal", actor.Short(principal), "repo", owner+"/"+repo, "error", err, principalNameAttr(r.Context()))
	}
}

// acceptsDefaultJSON reports whether the request asks for GitHub's default
// JSON representation — the only the cache models. Media types that
// change the response shape (application/vnd.github.raw, .html, .object,
// .diff, ...) make the route pass through instead. Every listed media range
// must be a JSON-default; an empty Accept means "anything" and is fine.
func acceptsDefaultJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if strings.TrimSpace(accept) == "" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		mediaType := strings.TrimSpace(strings.ToLower(part))
		if i := strings.IndexByte(mediaType, ';'); i >= 0 {
			mediaType = strings.TrimSpace(mediaType[:i])
		}
		switch mediaType {
		case "*/*", "application/*", "application/json",
			"application/vnd.github+json", "application/vnd.github.v3+json":
			// JSON-default representation.
		default:
			return false
		}
	}
	return true
}

// fetchUpstream forwards the (buffered-body) request to GitHub with the
// caller's own headers and returns the response plus its buffered body.
// overflow reports that the body exceeded maxAbsorbBodyBytes — the remainder
// is still readable from resp.Body, and such a response must be replayed, not
// absorbed. The URL is rebuilt from the request's escaped path + raw query so
// encoding reaches GitHub exactly as the caller sent it.
func (h *handlers) fetchUpstream(r *http.Request, body []byte) (*http.Response, []byte, bool, error) {
	target := h.gh.BaseURL() + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	// The real mirror→GitHub leg is charted by the client's own transport
	ctx := withUpstreamDisposition(r.Context(), dispUpstream)
	req, err := http.NewRequestWithContext(ctx, r.Method, target, rd)
	if err != nil {
		return nil, nil, false, err
	}
	copyForwardHeaders(req.Header, r.Header)

	who := callerLabel(r)
	resp, err := h.upstream.Do(req)
	if err != nil {
		return nil, nil, false, err
	}
	// Passively record the X-RateLimit-* headers on every cached-route miss
	h.meter.Observe(who.Key, who.Name, resp)
	invalidateMintOnAuthFailure(r.Context(), h.store, bearerToken(r), resp)
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxAbsorbBodyBytes+1))
	if err != nil {
		resp.Body.Close()
		return nil, nil, false, err
	}
	overflow := false
	if len(buf) > maxAbsorbBodyBytes {
		overflow = true
	}
	return resp, buf, overflow, nil
}

// hopByHopHeaders are connection-scoped request headers never forwarded
// upstream (per RFC); Accept-Encoding is also dropped so the transport
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Accept-Encoding",
}

func copyForwardHeaders(dst, src http.Header) {
	for k, vv := range src {
		dst[k] = append([]string(nil), vv...)
	}
	for _, k := range hopByHopHeaders {
		dst.Del(k)
	}
}

// replayUnstored writes an upstream response the cache could not absorb back
// to the client — status, headers (minus GitHub's CORS copies, which the
// mirror's corsMiddleware owns), and body — and records it as a passthrough:
// it was forwarded, uncached.
func (h *handlers) replayUnstored(w http.ResponseWriter, r *http.Request, resp *http.Response, body []byte) {
	_ = stripUpstreamCORS(resp)
	for k, vv := range resp.Header {
		w.Header()[k] = append([]string(nil), vv...)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	// A response larger than the absorb buffer streams its tail through.
	_, _ = io.Copy(w, resp.Body)
	// The REQUEST was modeled; the RESPONSE was not (an unexpected status, an
	h.reqlog.observeStatus(r.WithContext(withPassthroughReason(r.Context(), PassResponse)), DispPassthrough, resp.StatusCode)
	// This path never reaches recordPassthrough's sampler, and it is exactly
	route := normalizeRoute(r.URL.Path)
	var sample []byte
	if h.shapes.wantsBody(r.Method, route) && len(body) <= shapeMaxSampleBytes {
		sample = body
	}
	h.shapes.observeRequest(r, route, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), sample)
}

// upstreamError reports a failed upstream fetch, mirroring the passthrough
// proxy's error handling.
func (h *handlers) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Warn("cached route upstream fetch failed", "method", r.Method, "path", r.URL.Path, "error", err)
	h.reqlog.observeStatus(r, DispError, http.StatusBadGateway)
	http.Error(w, "bad gateway", http.StatusBadGateway)
}

// writeRebuilt writes a rebuilt (trimmed) JSON response with the cache marker.
func writeRebuilt(w http.ResponseWriter, status int, body []byte, hit bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if hit {
		w.Header().Set(cacheHeader, "hit")
	} else {
		w.Header().Set(cacheHeader, "miss")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// marshalTrimmed encodes a rebuilt body. It delegates to the storage layer's
func marshalTrimmed(v interface{}) ([]byte, error) {
	return ghdata.MarshalCacheDoc(v)
}
