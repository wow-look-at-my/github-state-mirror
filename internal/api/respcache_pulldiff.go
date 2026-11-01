// pullDiff406TTL backstops a stale verdict; webhooks flush sooner.

package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// pullDiff406TTL backstops a stale verdict; webhooks flush sooner.
const pullDiff406TTL = 24 * time.Hour

// acceptsPullDiff reports the exact unified-diff Accept pr-minder sends; multiple ranges keep the plain passthrough.
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

// cachedPullDiff reuses cachedPull's exact deny kind and resource key: the diff is the same resource, same authorization.
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
	diffKey := ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: ghdata.RepoAnswerKindPullDiff, RefID: numStr}
	if a, ok, err := h.store.GetRepoAnswer(r.Context(), diffKey, now); err != nil {
		slog.Warn("pull diff cache read failed", "owner", owner, "repo", repo, "number", number, "error", err)
	} else if ok {
		h.reqlog.observe(r, DispHit)
		writePullDiff(w, a.Doc, true)
		return
	}
	if doc, ok, err := h.store.GetCachedPullDiff406(r.Context(), owner, repo, number, now); err != nil {
		slog.Warn("pull diff 406 cache read failed", "owner", owner, "repo", repo, "number", number, "error", err)
	} else if ok {
		h.reqlog.observeStatus(r, DispHit, http.StatusNotAcceptable)
		writeRebuilt(w, http.StatusNotAcceptable, []byte(doc), true)
		return
	}
	if h.servePullVerdict(w, r, owner, repo, numStr) {
		return
	}

	// fetchUpstream forwards the inbound headers, so the diff Accept reaches GitHub.
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	if h.storePullVerdict(w, r, owner, repo, numStr, resp, body, overflow) {
		return
	}

	if !overflow && resp.StatusCode == http.StatusOK && len(body) <= pullDiffMaxBytes {
		// The diff is opaque text, stored VERBATIM under the identical-or-passthrough rule.
		a := ghdata.RepoAnswer{Status: http.StatusOK, Doc: string(body), Subject: numStr, ExpiresAt: now.Add(pullDiff406TTL)}
		if err := h.store.PutRepoAnswer(r.Context(), diffKey, a, now); err != nil {
			slog.Warn("pull diff cache write failed", "owner", owner, "repo", repo, "number", number, "error", err)
		}
		h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
		h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
		writePullDiff(w, a.Doc, false)
		return
	}
	if overflow || resp.StatusCode != http.StatusNotAcceptable {
		// Any other status is never stored; a 2xx is still fresh proof of access.
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

// pullDiff406JSON trims a verdict to GitHub's message; the consumer branches on the status, never the body.
type pullDiff406JSON struct {
	Message string `json:"message"`
}

// upstream406Message extracts GitHub's error message, falling back to the status text.
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

// pullDiffMaxBytes caps a stored diff at the absorb read limit; a larger one overflows and relays unstored.
const pullDiffMaxBytes = maxAbsorbBodyBytes

// writePullDiff serves a stored diff byte for byte, under the media type
// GitHub answers it with.
func writePullDiff(w http.ResponseWriter, diff string, hit bool) {
	w.Header().Set("Content-Type", "application/vnd.github.diff; charset=utf-8")
	if hit {
		w.Header().Set(cacheHeader, "hit")
	} else {
		w.Header().Set(cacheHeader, "miss")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(diff))
}
