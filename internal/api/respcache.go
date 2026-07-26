package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file implements the cached REST routes. The contract (see CLAUDE.md,
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
//   - GET /repos/{owner}/{repo}/contents/{path...}  (200 file/dir AND 404)
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
func (h *handlers) cachedContents(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	path := chi.URLParam(r, "*")

	// Only the plain JSON representation is absorbed. Other Accept media types
	// (raw/html/object) change the response shape entirely — passthrough.
	if !acceptsDefaultJSON(r) {
		h.ghProxy.ServeHTTP(w, r)
		return
	}
	// The contents endpoint takes exactly one query param, ref. Anything else
	// is a shape we don't model — passthrough.
	q := r.URL.Query()
	ref := q.Get("ref")
	delete(q, "ref")
	if len(q) > 0 {
		h.ghProxy.ServeHTTP(w, r)
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
		h.serveContents(w, r, c, true)
		return
	} else if err != nil {
		slog.Warn("contents cache read failed", "owner", owner, "repo", repo, "path", path, "error", err)
	}

	// Miss: fetch from GitHub with the caller's own credentials.
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	c, absorbed := absorbContents(owner, repo, path, ref, resp.StatusCode, body)
	if overflow || !absorbed {
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
	h.serveContents(w, r, c, false)
}

// serveContents rebuilds and writes the trimmed contents response.
func (h *handlers) serveContents(w http.ResponseWriter, r *http.Request, c ghdata.CachedContents, hit bool) {
	status, body, err := renderContents(c)
	if err != nil {
		slog.Warn("contents cache render failed", "path", c.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hit {
		h.reqlog.observe(r, DispHit)
	}
	writeRebuilt(w, status, body, hit)
}

// contentsFileJSON is the trimmed rebuild of a file response: GitHub's shape
// minus url/git_url/html_url/download_url/_links.
type contentsFileJSON struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Size     int64  `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Content  string `json:"content"`
	SHA      string `json:"sha"`
}

// contentsEntryJSON is one trimmed directory-listing entry.
type contentsEntryJSON struct {
	Type string `json:"type"`
	Size int64  `json:"size"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
}

// notFoundJSON is the trimmed rebuild of a 404: GitHub's message + status,
// documentation_url dropped.
type notFoundJSON struct {
	Message string `json:"message"`
	Status  string `json:"status"`
}

// absorbContents parses an upstream contents response into cacheable state.
// It absorbs a 200 file (base64-encoded — the >1 MiB "encoding":"none" form
// is not modeled), a 200 directory listing, and a 404. Anything else — other
// statuses, symlink/submodule objects, unexpected shapes — reports false and
// is served verbatim, unstored.
func absorbContents(owner, repo, path, ref string, status int, body []byte) (ghdata.CachedContents, bool) {
	c := ghdata.CachedContents{Owner: owner, Repo: repo, Path: path, Ref: ref}
	switch status {
	case http.StatusOK:
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 {
			return c, false
		}
		if trimmed[0] == '[' { // directory listing
			var raw []struct {
				Type string `json:"type"`
				Size int64  `json:"size"`
				Name string `json:"name"`
				Path string `json:"path"`
				SHA  string `json:"sha"`
			}
			if err := json.Unmarshal(trimmed, &raw); err != nil {
				return c, false
			}
			entries := make([]contentsEntryJSON, 0, len(raw))
			for _, e := range raw {
				entries = append(entries, contentsEntryJSON(e))
			}
			rendered, err := marshalTrimmed(entries)
			if err != nil {
				return c, false
			}
			c.Kind = ghdata.ContentsKindDir
			c.Entries = string(rendered)
			return c, true
		}
		var f struct {
			Type     string  `json:"type"`
			Encoding string  `json:"encoding"`
			Size     int64   `json:"size"`
			Name     string  `json:"name"`
			Path     string  `json:"path"`
			Content  *string `json:"content"`
			SHA      string  `json:"sha"`
		}
		if err := json.Unmarshal(trimmed, &f); err != nil {
			return c, false
		}
		if f.Type != "file" || f.Encoding != "base64" || f.Content == nil || f.SHA == "" {
			return c, false
		}
		c.Kind = ghdata.ContentsKindFile
		c.Name, c.SHA, c.Size, c.Encoding, c.Content = f.Name, f.SHA, f.Size, f.Encoding, *f.Content
		return c, true
	case http.StatusNotFound:
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Message == "" {
			e.Message = "Not Found"
		}
		c.Kind = ghdata.ContentsKindMissing
		c.Message = e.Message
		return c, true
	default:
		return c, false
	}
}

// renderContents rebuilds the trimmed response body for absorbed contents
// state. Hits and misses both go through here, so the served shape is
// identical regardless of cache state.
func renderContents(c ghdata.CachedContents) (int, []byte, error) {
	switch c.Kind {
	case ghdata.ContentsKindFile:
		body, err := marshalTrimmed(contentsFileJSON{
			Type: "file", Encoding: c.Encoding, Size: c.Size,
			Name: c.Name, Path: c.Path, Content: c.Content, SHA: c.SHA,
		})
		return http.StatusOK, body, err
	case ghdata.ContentsKindDir:
		return http.StatusOK, []byte(c.Entries), nil
	case ghdata.ContentsKindMissing:
		body, err := marshalTrimmed(notFoundJSON{Message: c.Message, Status: "404"})
		return http.StatusNotFound, body, err
	default:
		return 0, nil, fmt.Errorf("unknown contents kind %q", c.Kind)
	}
}

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
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, rd)
	if err != nil {
		return nil, nil, false, err
	}
	copyForwardHeaders(req.Header, r.Header)

	// Time the real mirror→GitHub leg (headers through buffered body) into
	// the timeline ring as its own "upstream" event, distinct from the
	// inbound miss the route records end-to-end via observeStatus: both
	// exchanges are real, so both are on the chart.
	who := callerLabel(r)
	start := time.Now()
	recordFetch := func(status int, disp string) {
		h.timeline.RecordRequest(start, time.Since(start), r.Method, normalizeRoute(r.URL.Path), status, disp, who.Key, who.Name)
	}

	resp, err := h.upstream.Do(req)
	if err != nil {
		recordFetch(0, DispError)
		return nil, nil, false, err
	}
	// Passively record the X-RateLimit-* headers on every cached-route miss
	// fetch, labeled with the same identity the request log records.
	h.meter.Observe(who.Key, who.Name, resp)
	invalidateMintOnAuthFailure(r.Context(), h.store, bearerToken(r), resp)
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxAbsorbBodyBytes+1))
	if err != nil {
		recordFetch(resp.StatusCode, DispError)
		resp.Body.Close()
		return nil, nil, false, err
	}
	recordFetch(resp.StatusCode, dispUpstream)
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
	h.reqlog.observeStatus(r, DispPassthrough, resp.StatusCode)
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

// marshalTrimmed encodes a rebuilt body without HTML escaping (GitHub does
// not escape <, >, & in JSON, and commit messages routinely contain them) and
// without a trailing newline.
func marshalTrimmed(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
