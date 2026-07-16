package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file implements the single-PR DIFF read's cached 406 verdicts (tier 2
// of the cache contract; the branch off cachedPull in respcache_pulls.go):
//
//	GET /repos/{owner}/{repo}/pulls/{number}  with Accept: application/vnd.github.diff
//
// pr-minder's getPullDiff probes the unified diff first and branches ONLY on
// ok/406 (survey 2026-07-11) -- a 406 means "diff too large", triggering the
// files-API fallback -- and an oversized PR re-earns the same 406 on every
// describe hand-off. The 406 VERDICT is therefore cached
// (pull_diff406_cache, per PR). A 200 diff BODY is deliberately never
// stored: that would be verbatim byte caching, which the cache doctrine
// rejects (tier 2 absorbs state and rebuilds; an opaque diff has no state to
// absorb) -- so 200s and every other status relay unstored, every time.
//
// Flushes: pull_request/pull_request_review events flush one PR's verdict (a
// head push or retarget can shrink the diff back under the boundary);
// push/repository events flush the whole repo (a BASE push can move the
// three-dot diff across the boundary in either direction, with no per-PR
// signal); the 24h TTL backstops missed deliveries.

// pullDiff406TTL bounds a stale 406 verdict; webhooks flush sooner (see the
// file comment) and this is the backstop.
const pullDiff406TTL = 24 * time.Hour

// acceptsPullDiff reports whether the Accept header is EXACTLY one media
// range naming the unified-diff representation -- pr-minder's getPullDiff
// sends `Accept: application/vnd.github.diff` and nothing else (survey
// 2026-07-11); the v3-suffixed spelling is the same representation. An
// Accept listing multiple ranges is not that consumer shape and keeps the
// plain passthrough, like every other non-default Accept.
func acceptsPullDiff(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, ",") {
		return false
	}
	mediaType := strings.TrimSpace(strings.ToLower(accept))
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = strings.TrimSpace(mediaType[:i])
	}
	return mediaType == "application/vnd.github.diff" || mediaType == "application/vnd.github.v3.diff"
}

// cachedPullDiff serves a single-PR DIFF read's cached 406 verdict, fetching
// on a miss. Only the 406 "diff too large" answer is ever stored (see the
// file comment). The reveal gate reuses the EXISTING pull deny kind and
// cachedPull's exact resource key: the diff is the same underlying resource
// with the same authorization semantics, so a deny verdict earned on either
// representation answers both.
func (h *handlers) cachedPullDiff(w http.ResponseWriter, r *http.Request, owner, repo string, number int64, numStr string) {
	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindPull, ghdata.NormalizeRepoKey(owner)+"/"+ghdata.NormalizeRepoKey(repo)+"#"+numStr); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if doc, ok, err := h.store.GetCachedPullDiff406(r.Context(), owner, repo, number, now); err != nil {
		slog.Warn("pull diff 406 cache read failed", "owner", owner, "repo", repo, "number", number, "error", err)
	} else if ok {
		h.reqlog.observeStatus(r, DispHit, http.StatusNotAcceptable)
		writeRebuilt(w, http.StatusNotAcceptable, []byte(doc), true)
		return
	}

	// Miss: fetch with the caller's own headers -- fetchUpstream forwards the
	// inbound request's headers (copyForwardHeaders), so the diff Accept
	// reaches GitHub and the answer is the real diff-or-406 verdict.
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	if overflow || resp.StatusCode != http.StatusNotAcceptable {
		// A 200 diff (or any other status) is deliberately never stored --
		// see the file comment. A 2xx is still fresh proof of access.
		h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
		h.replayUnstored(w, r, resp, body)
		return
	}
	doc, mErr := marshalTrimmed(pullDiff406JSON{Message: upstream406Message(body)})
	if mErr != nil {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedPullDiff406(r.Context(), owner, repo, number, string(doc), now, pullDiff406TTL); err != nil {
		slog.Warn("pull diff 406 cache write failed", "owner", owner, "repo", repo, "number", number, "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusNotAcceptable, doc, false)
}

// pullDiff406JSON is the trimmed rebuild of a 406 "diff too large" verdict:
// GitHub's message only, documentation_url dropped. The consumer branches on
// the STATUS (406 -> files-API fallback), never the body.
type pullDiff406JSON struct {
	Message string `json:"message"`
}

// upstream406Message extracts GitHub's error message from a 406 body,
// falling back to the status text.
func upstream406Message(body []byte) string {
	msg := struct {
		Message string `json:"message"`
	}{}
	_ = json.Unmarshal(body, &msg)
	if msg.Message == "" {
		return "Not Acceptable"
	}
	return msg.Message
}
