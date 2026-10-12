package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The single workflow-run read, rebuilt from the workflow_runs truth row.

// cachedWorkflowRun serves GET /repos/{owner}/{repo}/actions/runs/{run_id}.
// workflow_run deliveries keep the row current. A miss fetches the whole run
// and applies it to truth like a delivery, then renders the row. A hit and a
// miss go through one render.
func (h *handlers) cachedWorkflowRun(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))

	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	runID, err := strconv.ParseInt(chi.URLParam(r, "run_id"), 10, 64)
	if err != nil || runID <= 0 {
		h.passthrough(w, r, PassPath)
		return
	}
	excludePRs, ok := parseWorkflowRunQuery(r)
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}

	resource := owner + "/" + repo + "/actions/runs/" + strconv.FormatInt(runID, 10)
	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindWorkflowRun, resource); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}
	if v, ok := h.subresourceVerdict(r, denyKindWorkflowRun, resource); ok {
		h.serveDenyVerdict(w, r, v, true)
		return
	}

	now := time.Now()
	if row, ok, err := h.store.GetWorkflowRun(r.Context(), owner, repo, runID); err != nil {
		slog.Warn("workflow run read failed", "owner", owner, "repo", repo, "run", runID, "error", err)
	} else if ok && ghdata.WorkflowRunServable(row, now) {
		if doc, ok := ghdata.RenderWorkflowRun(row, excludePRs); ok {
			h.reqlog.observe(r, DispHit)
			writeRebuilt(w, http.StatusOK, doc, true)
			return
		}
	}

	// The fetch drops exclude_pull_requests: truth stores the whole object, and the render applies the caller's choice.
	fetch := r.Clone(r.Context())
	fetch.URL.RawQuery = ""
	resp, body, overflow, err := h.fetchUpstream(fetch, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	if v, ok := h.recordSubresourceVerdict(r, owner, repo, denyKindWorkflowRun, resource, resp, body, http.StatusNotFound); ok {
		h.serveDenyVerdict(w, r, v, false)
		return
	}
	if overflow || resp.StatusCode != http.StatusOK {
		h.replayUnstored(w, r, resp, body)
		return
	}
	run, ok := ghdata.ParseWorkflowRunObject(owner, repo, body)
	if !ok || run.RunID != runID {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.ApplyWorkflowRun(r.Context(), run, now); err != nil {
		slog.Warn("workflow run absorb failed", "owner", owner, "repo", repo, "run", runID, "error", err)
		h.replayUnstored(w, r, resp, body)
		return
	}
	// The row can be newer than this answer: a delivery that landed first wins the updated_at comparison.
	row, ok, err := h.store.GetWorkflowRun(r.Context(), owner, repo, runID)
	if err != nil || !ok {
		h.replayUnstored(w, r, resp, body)
		return
	}
	doc, ok := ghdata.RenderWorkflowRun(row, excludePRs)
	if !ok {
		h.replayUnstored(w, r, resp, body)
		return
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, doc, false)
}

// parseWorkflowRunQuery reports exclude_pull_requests, the read's only
// documented parameter. Any other parameter, or another value, is unmodeled.
func parseWorkflowRunQuery(r *http.Request) (excludePRs bool, ok bool) {
	for key, vals := range r.URL.Query() {
		if key != "exclude_pull_requests" || len(vals) != 1 {
			return false, false
		}
		switch vals[0] {
		case "true":
			excludePRs = true
		case "false":
		default:
			return false, false
		}
	}
	return excludePRs, true
}
