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
// a TRIMMED response from that state — it deliberately does NOT replay
// GitHub's bytes. Every URL field (url, *_url, _links) is dropped from
// rebuilt bodies; consumers are first-party tooling that reads state fields
// only. Hits and misses both serve the rebuilt shape, so a route's shape
// never flip-flops with cache state. Anything a route cannot absorb (an
// unexpected shape, a non-cacheable status, a non-JSON Accept) is forwarded
// or replayed verbatim, unstored, and recorded as a passthrough.
//
// Cached routes:
//
//   - GET /repos/{owner}/{repo}/contents/{path...}  (respcache_contents.go;
//     200 file/dir AND 404; the default JSON shape AND the raw file-body
//     Accept share one absorbed row — see that file's header)
//   - GET /repos/{owner}/{repo}/git/commits/{sha}   (respcache_gitcommits.go;
//     200 immutable + expiring 404 miss markers)
//   - POST /app/installations/{id}/access_tokens    (201; App-JWT verified)
//   - GET /repos/{owner}/{repo}/pulls               (respcache_pulls.go)
//   - GET /repos/{owner}/{repo}/pulls/{number}      (respcache_pulls.go; the
//     diff-Accept read's 406 verdicts in respcache_pulldiff.go)
//   - GET /repos/{owner}/{repo}/installation        (respcache_pulls.go)
//   - GET /repos/{owner}/{repo}/commits             (respcache_commits.go)
//   - GET /repos/{owner}/{repo}/compare/{basehead}  (respcache_compare.go)
//   - GET /repos/{owner}/{repo}/commits/{ref}/status      (respcache_commitci.go)
//   - GET /repos/{owner}/{repo}/commits/{ref}/check-runs  (respcache_commitci.go)
//   - GET /repos/{owner}/{repo}/commits/{ref}/statuses    (respcache_commitci.go)
//   - GET /repos/{owner}/{repo}/statuses/{ref}      (its legacy alias, same file)
//   - GET /repos/{owner}/{repo}/actions/runs        (respcache_actionsruns.go)
//   - GET /repos/{owner}/{repo}                     (respcache_repo.go)
//   - GET /repos/{owner}/{repo}/branches            (respcache_branches.go)
//   - GET /repos/{owner}/{repo}/pulls/{number}/files (respcache_pullfiles.go)
//   - GET /repos/{owner}/{repo}/labels/{name}       (respcache_labels.go)
//   - GET /installation/repositories                (respcache_installationrepos.go;
//     keyed by the BEARER, not the principal — that answer is one token's own)
//   - GET /repos/{owner}/{repo}/hooks               (respcache_hooks.go)
//   - GET /orgs/{org}/hooks                         (same file; both keyed by the
//     BEARER because they are ADMIN-only reads and the reveal layer proves READ)
//
// The single-PR route was once deliberately passthrough because its body
// carries the lazily-computed `mergeable` field that pr-minder polls for; it
// is now cached behind a known-mergeable gate — an unknown/null mergeable
// ALWAYS misses, so the resolve-poll still reaches GitHub (respcache_pulls.go).

const (
	// contentsCacheTTL is the TTL backstop on cached contents rows. Webhooks
	// (push/repository) invalidate much sooner; the TTL only bounds how long a
	// MISSED webhook could serve stale state. Git commits are immutable and
	// have no TTL; token mints expire with the token.
	contentsCacheTTL = 24 * time.Hour

	// mintExpiryBuffer is subtracted from a minted token's expires_at to get
	// the serve-until time: a cached mint is never served within 10 minutes of
	// the token's real expiry, so callers always have usable lifetime left.
	mintExpiryBuffer = 10 * time.Minute

	// maxAbsorbBodyBytes caps how much of an upstream response the cached
	// routes buffer for absorption. A larger response is replayed verbatim,
	// unstored (contents API JSON tops out well below this).
	maxAbsorbBodyBytes = 8 << 20 // 8 MiB

	// maxMintBodyBytes caps the buffered token-mint request body (a
	// permissions/repositories JSON object; real ones are tiny).
	maxMintBodyBytes = 1 << 20 // 1 MiB

	// cacheHeader marks responses served by a cached route: "hit" (rebuilt
	// from stored state, no upstream call) or "miss" (fetched, absorbed, then
	// rebuilt). Passthrough responses carry no marker.
	cacheHeader = "X-GSM-Cache"
)

// ---- GET /repos/{owner}/{repo}/contents/{path...} ----

// cachedContents serves repo contents from absorbed state, fetching and
// absorbing on a miss. Cache key: (actor, owner, repo, path, ref) — the raw
// `ref` query value matters (`contents?ref=...` differs per ref). Both 200
// (file or directory) and 404 ("config file absent" — half the win for
// pr-minder's per-repo config probe) are absorbed.
//
// TWO Accept shapes are modeled from the SAME absorbed row: the default JSON
// representation (base64 `content` field) and the raw file-body
// representation (Accept: application/vnd.github.raw, what simple-llm-ui's
// file-read tool sends — docs/tools/filesystem-tools.md in that repo). They
// are the same underlying bytes, so there is no second cache dimension: a
// raw-Accept request is served by base64-DECODING the already-absorbed
// `content` (serveContentsRaw) rather than caching a second copy, and a miss
// is always fetched with the DEFAULT JSON Accept regardless of which shape
// the caller asked for — mirroring go-github, PyGithub and octokit.js, which
// all resolve file-vs-directory via the default JSON shape first and never
// send the raw media type against a path that might be a directory (GitHub's
// behavior there is undocumented). The one place this can't be made to work
// -- a file too large for the base64 JSON form (>1 MiB; GitHub answers
// encoding:"none" past that, only raw/object media types carry the bytes,
// per docs.github.com/en/rest/repos/contents) -- falls back to a second,
// uncached fetchUpstream call with the caller's own raw Accept header,
// replayed verbatim (replayUnstored) rather than routed through the
// passthrough proxy's debouncer, which exists to price shapes this mirror
// could still learn to model, not ones that are structurally unabsorbable.
func (h *handlers) cachedContents(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	path := chi.URLParam(r, "*")

	// Only the default JSON representation and the raw file-body
	// representation are absorbed (see the file-header comment above). Any
	// other Accept media type (html/object, or an ambiguous mix) changes the
	// response shape entirely — passthrough.
	rawAccept := acceptsRawContents(r)
	if !acceptsDefaultJSON(r) && !rawAccept {
		h.passthrough(w, r, PassAccept)
		return
	}
	// The contents endpoint takes exactly one query param, ref. Anything else
	// is a shape we don't model — passthrough.
	q := r.URL.Query()
	ref := q.Get("ref")
	delete(q, "ref")
	if len(q) > 0 {
		h.passthrough(w, r, PassQuery)
		return
	}

	// Reveal: may this caller read the repo's cached state?
	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindContents, contentsResourceKey(owner, repo, path, ref)); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if c, ok, err := h.store.GetCachedContents(r.Context(), owner, repo, path, ref, now); err == nil && ok {
		h.serveContentsHit(w, r, c, rawAccept)
		return
	} else if err != nil {
		slog.Warn("contents cache read failed", "owner", owner, "repo", repo, "path", path, "error", err)
	}

	// Miss: fetch from GitHub with the caller's own credentials. A raw-Accept
	// request is still probed with the DEFAULT JSON Accept (see the file
	// header) — only the caller's Authorization and other headers ride along
	// unchanged.
	probeReq := r
	if rawAccept {
		probeReq = r.Clone(r.Context())
		probeReq.Header = r.Header.Clone()
		probeReq.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, body, overflow, err := h.fetchUpstream(probeReq, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	c, absorbed := absorbContents(owner, repo, path, ref, resp.StatusCode, body)
	if overflow || !absorbed {
		if rawAccept {
			// The JSON probe couldn't carry this response — most likely a
			// file over the ~1 MiB base64 threshold, which GitHub answers
			// with encoding:"none" and no content past that size (only
			// raw/object media types work there). Fetch again with the
			// caller's OWN raw Accept header (r, unmodified) and replay it
			// verbatim via fetchUpstream/replayUnstored directly — NOT
			// h.passthrough, which would route this second call through the
			// passthrough proxy's debouncer: that hold exists to price
			// shapes this mirror could still learn to model, not ones that
			// are structurally unabsorbable regardless of Accept.
			rawResp, rawBody, _, rawErr := h.fetchUpstream(r, nil)
			if rawErr != nil {
				h.upstreamError(w, r, rawErr)
				return
			}
			defer rawResp.Body.Close()
			h.replayUnstored(w, r, rawResp, rawBody)
			return
		}
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedContents(r.Context(), c, now, contentsCacheTTL); err != nil {
		slog.Warn("contents cache write failed", "owner", owner, "repo", repo, "path", path, "error", err)
	}
	// A 2xx with the caller's own token is fresh proof of access -- renew the
	// grant so steady consumers never age out mid-use. (A 404 is not proof
	// either way; the reveal layer already vouched for this read.)
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	if rawAccept && c.Kind == ghdata.ContentsKindFile {
		h.serveContentsRaw(w, r, c, false)
		return
	}
	h.serveContents(w, r, c, false)
}

// absorbContents, renderContents, serveContents, and the contents JSON types
// live in respcache_contents.go (this file was over the 500-line warning
// threshold once raw-Accept support landed; the contents shapes and their
// absorption/render are one natural unit, kept together there).

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
// JSON representation — the only one the cache models. Media types that
// change the response shape (application/vnd.github.raw, .html, .object,
// .diff, ...) make the route pass through instead. Every listed media range
// must be a JSON-default one; an empty Accept means "anything" and is fine.
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
	// (TimelineUpstreamObserver), distinct from the inbound miss the route
	// records end-to-end via observeStatus: both exchanges are real, so both
	// are on the chart. The context says which kind of call this is; the
	// transport does the rest, so a future caller of h.upstream is charted
	// without having to remember to be.
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
	// fetch, labeled with the same identity the request log records.
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
// upstream (per RFC 9110); Accept-Encoding is also dropped so the transport
// negotiates (and transparently decodes) compression itself, keeping buffered
// bodies plain bytes.
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
	// oversized body, a symlink/submodule object). Unlike every shape-guard
	// passthrough this one already cost an upstream round trip.
	h.reqlog.observeStatus(r.WithContext(withPassthroughReason(r.Context(), PassResponse)), DispPassthrough, resp.StatusCode)
	// This path never reaches recordPassthrough's sampler, and it is exactly
	// the class whose ANSWER the brief most needs — the route models the
	// request but not what came back. The body is already buffered here.
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
// renderer because a stored doc a webhook rewrote in place must be byte-equal
// to the one this layer would render (ghdata.MarshalCacheDoc).
func marshalTrimmed(v interface{}) ([]byte, error) {
	return ghdata.MarshalCacheDoc(v)
}
