package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file implements the cached workflow-runs route (tier 2 of the cache
// contract, like respcache.go):
//
//	GET /repos/{owner}/{repo}/actions/runs?head_sha=<sha>
//
// The per-sha runs listing is what both mirror-pointed consumers poll
// (survey 2026-07-11): pr-minder's hasWorkflowRuns sends
// `?head_sha=<hex>&per_page=1` and reads ONLY `total_count` (the zombie-PR
// probe, repeated per bot PR by the reconcile hook's fleet sweep), and
// required-builds' listWorkflowRuns pages `?head_sha=<hex>&per_page=100&
// page=N` reading name/status/conclusion/html_url. Between CI events a sha's
// listing is stable, so the whole trimmed page is snapshotted per exact
// request (owner, repo, head_sha, per_page, page).
//
// head_sha is REQUIRED for a cacheable shape: an UNFILTERED runs listing
// churns with every run anywhere in the repo and no consumer sends it, so it
// is deliberately unmodeled (passthrough) rather than a cache row that would
// be stale on arrival. Every other filter param (branch, event, status,
// actor, created, exclude_pull_requests, ...) changes the body's contents
// and passes through too. Deeper /actions/runs/{id}/... paths never reach
// this route (the registration is the exact literal) and keep falling to the
// NotFound passthrough.
//
// Staleness: run creation and state changes fire check_suite/check_run/
// workflow_job events within seconds, and the round-2 dispatcher flushes the
// sha's pages on all of them (workflow_job deliveries flush even when the
// queued/waiting disposition drops them, since invalidation precedes the
// disposition logic). Run DELETION emits no webhook at all, so a deleted
// run's page can only age out via the 24h TTL -- acceptable because both
// consumers fail safe on a stale answer (pr-minder's zombie revive is
// additionally guarded by its commit-age deferral, and required-builds
// re-aggregates on the next event). repository events flush repo-wide like
// every response cache.

const (
	// workflowRunsCacheTTL bounds how long a stale runs page can be served.
	// CI webhooks flush within seconds of any run change; the TTL is the
	// backstop for the one signal GitHub never webhooks -- run DELETION --
	// and for missed deliveries.
	workflowRunsCacheTTL = 24 * time.Hour

	// workflowRunsDefaultPerPage is GitHub's default page size for the runs
	// listing when the request does not send per_page.
	workflowRunsDefaultPerPage = 30

	// workflowRunsMaxCachedPage caps which pages are modeled. A sha rarely
	// has more than a handful of runs; deeper pagination passes through.
	workflowRunsMaxCachedPage = 10
)

// parseWorkflowRunsShape reports the shape of an /actions/runs query and
// whether the cache models it: head_sha REQUIRED (non-empty, a full hex
// object id after lowercasing -- the returned value is the normalized form),
// plus the standard per_page/page bounds. Unknown params, repeated params,
// or an absent/malformed head_sha make it non-cacheable.
func parseWorkflowRunsShape(q url.Values) (headSHA string, perPage, page int, ok bool) {
	perPage, page = workflowRunsDefaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return "", 0, 0, false
		}
		v := vals[0]
		switch key {
		case "head_sha":
			headSHA = strings.ToLower(v)
			if !isFullHexSHA(headSHA) {
				return "", 0, 0, false
			}
		case "per_page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return "", 0, 0, false
			}
			perPage = n
		case "page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > workflowRunsMaxCachedPage {
				return "", 0, 0, false
			}
			page = n
		default:
			return "", 0, 0, false
		}
	}
	if headSHA == "" {
		// No head_sha at all: the unfiltered listing, deliberately unmodeled
		// (see the file comment).
		return "", 0, 0, false
	}
	return headSHA, perPage, page, true
}

// cachedWorkflowRuns serves one page of a sha's workflow-runs listing from a
// stored whole-doc snapshot, fetching and absorbing on a miss. Shapes the
// cache does not model pass through.
func (h *handlers) cachedWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))

	if !acceptsDefaultJSON(r) {
		h.ghProxy.ServeHTTP(w, r)
		return
	}
	headSHA, perPage, page, ok := parseWorkflowRunsShape(r.URL.Query())
	if !ok {
		h.ghProxy.ServeHTTP(w, r)
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindWorkflowRuns, owner+"/"+repo+"/actions/runs@"+headSHA); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if doc, ok, err := h.store.GetCachedWorkflowRuns(r.Context(), owner, repo, headSHA, perPage, page, now); err != nil {
		slog.Warn("workflow runs cache read failed", "owner", owner, "repo", repo, "head_sha", headSHA, "error", err)
	} else if ok {
		h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, DispHit)
		writeRebuilt(w, http.StatusOK, []byte(doc), true)
		return
	}

	// Miss: fetch from GitHub with the caller's own credentials.
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	doc, absorbed := absorbWorkflowRuns(resp.StatusCode, body)
	if overflow || !absorbed {
		// Includes 404 and 5xx: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedWorkflowRuns(r.Context(), owner, repo, headSHA, perPage, page, doc, now, workflowRunsCacheTTL); err != nil {
		slog.Warn("workflow runs cache write failed", "owner", owner, "repo", repo, "head_sha", headSHA, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// workflowRunItemJSON is one trimmed entry of the workflow_runs array: the
// state fields the consumers read (required-builds: name/status/conclusion/
// html_url). name/conclusion/run_started_at are nullable and the keys are
// always emitted, exactly as upstream; html_url is a PINNED consumer-read
// exception to the no-URL doctrine (required-builds links the run's page
// from its breakdown). Dropped: node_id, the head_branch/event/actor/
// repository/head_commit objects, every other *_url, and the unbounded
// pull_requests/referenced_workflows arrays.
type workflowRunItemJSON struct {
	ID           int64   `json:"id"`
	Name         *string `json:"name"` // nullable (a run may have no name)
	HeadSHA      string  `json:"head_sha"`
	Status       string  `json:"status"`
	Conclusion   *string `json:"conclusion"` // nullable until completed
	HTMLURL      string  `json:"html_url"`   // pinned consumer-read exception
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	RunStartedAt *string `json:"run_started_at"` // nullable while queued
}

// workflowRunsJSON is the trimmed rebuild of a runs listing: {total_count,
// workflow_runs: [...]}. total_count is copied VERBATIM from upstream -- it
// is GitHub's TOTAL matching-run count, NOT the page length (pr-minder's
// hasWorkflowRuns sends per_page=1 and reads exactly this field as its
// "does the sha have any runs?" answer).
type workflowRunsJSON struct {
	TotalCount   int64                 `json:"total_count"`
	WorkflowRuns []workflowRunItemJSON `json:"workflow_runs"`
}

// absorbWorkflowRuns parses an /actions/runs 200 into the trimmed document
// (rendered once here; hits serve the stored bytes). Reports false -- serve
// verbatim, store nothing -- for any other status or any shape the model
// cannot hold: total_count and the workflow_runs array must both be PRESENT,
// and every run must carry a positive id, a status, and a full-hex head sha.
// An empty workflow_runs with total_count 0 is a valid, cacheable answer
// (exactly the "no runs yet" verdict the zombie probe is after).
func absorbWorkflowRuns(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false
	}
	var raw struct {
		TotalCount   *int64 `json:"total_count"`
		WorkflowRuns *[]struct {
			ID           int64   `json:"id"`
			Name         *string `json:"name"`
			HeadSHA      string  `json:"head_sha"`
			Status       string  `json:"status"`
			Conclusion   *string `json:"conclusion"`
			HTMLURL      string  `json:"html_url"`
			CreatedAt    string  `json:"created_at"`
			UpdatedAt    string  `json:"updated_at"`
			RunStartedAt *string `json:"run_started_at"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.TotalCount == nil || raw.WorkflowRuns == nil {
		return "", false
	}
	doc := workflowRunsJSON{
		TotalCount:   *raw.TotalCount,
		WorkflowRuns: make([]workflowRunItemJSON, 0, len(*raw.WorkflowRuns)),
	}
	for _, run := range *raw.WorkflowRuns {
		sha := strings.ToLower(run.HeadSHA)
		if run.ID <= 0 || run.Status == "" || !isFullHexSHA(sha) {
			return "", false
		}
		doc.WorkflowRuns = append(doc.WorkflowRuns, workflowRunItemJSON{
			ID: run.ID, Name: run.Name, HeadSHA: sha, Status: run.Status,
			Conclusion: run.Conclusion, HTMLURL: run.HTMLURL,
			CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, RunStartedAt: run.RunStartedAt,
		})
	}
	rendered, err := marshalTrimmed(doc)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
