package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file implements the cached SINGLE check-run route (tier of the
// cache contract):
//
//	GET /repos/{owner}/{repo}/check-runs/{check_run_id}
//
// Distinct from commit_ci_cache's LIST route
// (GET .../commits/{ref}/check-runs, respcache_commitci.go): this route is
// keyed by the check run's own numeric id rather than a ref, so it is always
// DIRECTLY appliable from a `check_run` delivery -- the payload carries the
// run's whole current state and IS the answer for that id (see
// ghdata.ApplyCheckRunByID). The reveal gate reuses denyKindCheckRuns's
// deny_cache SLOT namespace (denyKindSingleCheckRun) since a single run has
// the same repo-level authorization semantics as the listing.

// cachedCheckRun serves a single check run from absorbed state.
func (h *handlers) cachedCheckRun(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	idStr := chi.URLParam(r, "check_run_id")

	checkRunID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || checkRunID <= 0 || r.URL.RawQuery != "" || !acceptsDefaultJSON(r) {
		h.passthrough(w, r, shapeReason(r, err == nil && checkRunID > 0))
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindSingleCheckRun, owner+"/"+repo+"#"+idStr); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if doc, ok, err := h.store.GetCachedCheckRun(r.Context(), owner, repo, checkRunID, now); err != nil {
		slog.Warn("check run cache read failed", "owner", owner, "repo", repo, "id", checkRunID, "error", err)
	} else if ok {
		h.reqlog.observe(r, DispHit)
		writeRebuilt(w, http.StatusOK, []byte(doc), true)
		return
	}

	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	if overflow || resp.StatusCode != http.StatusOK {
		h.replayUnstored(w, r, resp, body)
		return
	}
	run, ok := ghdata.TrimCheckRunJSON(body)
	if !ok {
		h.replayUnstored(w, r, resp, body)
		return
	}
	doc, mErr := marshalTrimmed(run)
	if mErr != nil {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedCheckRun(r.Context(), owner, repo, checkRunID, string(doc), now, ghdata.CheckRunCacheTTL); err != nil {
		slog.Warn("check run cache write failed", "owner", owner, "repo", repo, "id", checkRunID, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, doc, false)
}
