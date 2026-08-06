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
//	GET /repos/{owner}/{repo}/actions/runs[?head_sha=|?status=&branch=]
//
// Two request shapes, one table, keyed apart by (head_sha, filters):
//
//   - The per-COMMIT listing (`?head_sha=<hex>`), what both mirror-pointed
//     consumers poll (survey 2026-07-11): pr-minder's hasWorkflowRuns sends
//     `?head_sha=<hex>&per_page=1` and reads ONLY `total_count` (the
//     zombie-PR probe, repeated per bot PR by the reconcile hook's fleet
//     sweep), and required-builds' listWorkflowRuns pages
//     `?head_sha=<hex>&per_page=100&page=N` reading name/status/conclusion/
//     html_url.
//   - The repo-wide LISTING (`?status=queued&per_page=N`, optionally
//     `branch=`), the GHA runner coordinator's queued-backlog poll.
//
// Every other filter (event, actor, created, exclude_pull_requests,
// check_suite_id, ...) is unmodeled and passes through, as do repeated
// params and out-of-range paging. Deeper /actions/runs/{id}/... paths never
// reach this route (the registration is the exact literal) and keep falling
// to the NotFound passthrough.
//
// Invalidation is what makes the LISTING safe to store, and it is the whole
// argument. A queued-backlog answer names no sha, so the per-sha flush that
// covers the commit shape cannot reach it -- instead every run-state
// delivery (status/check_run/check_suite/workflow_job/workflow_run) flushes
// EVERY listing row for the repo. Those deliveries cover both edges of
// `queued`: a run enters it via check_suite/check_run created +
// workflow_job queued (invalidation precedes the disposition logic, so the
// queued/waiting actions onWorkflowJob drops as ignored still flush), and
// leaves it via the same events' in_progress/completed actions.
//
// What the TTL is actually for: run DELETION, which GitHub delivers no
// webhook for at all, plus any missed delivery. For the commit shape that
// stays 24h -- both consumers fail safe on a stale answer (pr-minder's
// zombie revive is additionally guarded by its commit-age deferral, and
// required-builds re-aggregates on the next event). For the LISTING it is
// deliberately minutes: a phantom queued run is what a runner coordinator
// would over-provision against, so the un-delivered case is bounded tight
// even though it costs a re-fetch on an otherwise-quiet repo.

const (
	// workflowRunsCacheTTL bounds how long a stale per-COMMIT runs page can
	// be served. CI webhooks flush within seconds of any run change; the TTL
	// is the backstop for the one signal GitHub never webhooks -- run
	// DELETION -- and for missed deliveries.
	workflowRunsCacheTTL = 24 * time.Hour

	// workflowRunsListingTTL is the same backstop for the repo-wide LISTING,
	// held far shorter: this shape answers "what work is queued right now",
	// and the un-delivered failure (a deleted run, a missed delivery) leaves
	// a phantom in the queue rather than a stale answer nobody acts on.
	workflowRunsListingTTL = 5 * time.Minute

	// workflowRunsDefaultPerPage is GitHub's default page size for the runs
	// listing when the request does not send per_page.
	workflowRunsDefaultPerPage = 30

	// workflowRunsMaxCachedPage caps which pages are modeled. A sha rarely
	// has more than a handful of runs; deeper pagination passes through.
	workflowRunsMaxCachedPage = 10

	// workflowRunsMaxBranchLen bounds the branch filter admitted into a
	// cache key. Git's own ref limit is far higher, but nothing near this
	// occurs and an unbounded key component is a cardinality footgun.
	workflowRunsMaxBranchLen = 255
)

// workflowRunStatuses is the set of values GitHub documents for the runs
// listing's `status` filter (it doubles as a conclusion filter). Validating
// against the closed set keeps the cache key's cardinality bounded and makes
// an unknown value pass through rather than mint a row nothing will read.
var workflowRunStatuses = map[string]bool{
	"completed": true, "action_required": true, "cancelled": true,
	"failure": true, "neutral": true, "skipped": true, "stale": true,
	"success": true, "timed_out": true, "in_progress": true, "queued": true,
	"requested": true, "waiting": true, "pending": true,
}

// workflowRunsShape is one modeled /actions/runs request. HeadSHA is ''
// for the repo-wide listing and Filters is '' when no modeled filter was
// sent; the pair is the row key, and HeadSHA == "" is also what selects the
// listing TTL and the repo-wide listing flush.
type workflowRunsShape struct {
	HeadSHA string
	Filters string
	PerPage int
	Page    int
}

// listing reports whether this is the repo-wide shape (no head_sha), whose
// staleness rules differ from a single commit's.
func (s workflowRunsShape) listing() bool { return s.HeadSHA == "" }

// parseWorkflowRunsShape reports the shape of an /actions/runs query and
// whether the cache models it. head_sha (a full hex object id, lowercased),
// status (one of GitHub's documented values), and branch (a bounded ref
// name) are modeled, as are the standard per_page/page bounds; the modeled
// filters are canonicalized into one sorted, escaped key component. Unknown
// params, repeated params, or a malformed value make it non-cacheable.
func parseWorkflowRunsShape(q url.Values) (workflowRunsShape, bool) {
	shape := workflowRunsShape{PerPage: workflowRunsDefaultPerPage, Page: 1}
	filters := url.Values{}
	for key, vals := range q {
		if len(vals) != 1 {
			return workflowRunsShape{}, false
		}
		v := vals[0]
		switch key {
		case "head_sha":
			shape.HeadSHA = strings.ToLower(v)
			if !isFullHexSHA(shape.HeadSHA) {
				return workflowRunsShape{}, false
			}
		case "status":
			if !workflowRunStatuses[v] {
				return workflowRunsShape{}, false
			}
			filters.Set(key, v)
		case "branch":
			if v == "" || len(v) > workflowRunsMaxBranchLen || strings.ContainsFunc(v, isControlRune) {
				return workflowRunsShape{}, false
			}
			filters.Set(key, v)
		case "per_page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return workflowRunsShape{}, false
			}
			shape.PerPage = n
		case "page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > workflowRunsMaxCachedPage {
				return workflowRunsShape{}, false
			}
			shape.Page = n
		default:
			return workflowRunsShape{}, false
		}
	}
	// Encode sorts by key, so the canonical form does not depend on map
	// iteration order or on how the caller ordered its query string.
	shape.Filters = filters.Encode()
	return shape, true
}

// isControlRune reports whether r is a C0/C7F control character -- rejected
// from a cache key so a caller cannot smuggle a newline or NUL into stored
// state (the same stance as the repo's control-character CI check).
func isControlRune(r rune) bool { return r < 0x20 || r == 0x7f }

// cachedWorkflowRuns serves one page of a sha's workflow-runs listing from a
// stored whole-doc snapshot, fetching and absorbing on a miss. Shapes the
// cache does not model pass through.
func (h *handlers) cachedWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))

	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	shape, ok := parseWorkflowRunsShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindWorkflowRuns, workflowRunsResourceKey(owner, repo, shape)); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if doc, ok, err := h.store.GetCachedWorkflowRuns(r.Context(), owner, repo, shape.HeadSHA, shape.Filters, shape.PerPage, shape.Page, now); err != nil {
		slog.Warn("workflow runs cache read failed", "owner", owner, "repo", repo, "head_sha", shape.HeadSHA, "filters", shape.Filters, "error", err)
	} else if ok {
		h.reqlog.observe(r, DispHit)
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
	ttl := workflowRunsCacheTTL
	if shape.listing() {
		ttl = workflowRunsListingTTL
	}
	if err := h.store.PutCachedWorkflowRuns(r.Context(), owner, repo, shape.HeadSHA, shape.Filters, shape.PerPage, shape.Page, doc, now, ttl); err != nil {
		slog.Warn("workflow runs cache write failed", "owner", owner, "repo", repo, "head_sha", shape.HeadSHA, "filters", shape.Filters, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// workflowRunsResourceKey names the resource behind a runs request for the
// reveal layer's deny cache. The shape is part of the key so a denial
// recorded for one query cannot be replayed for another.
func workflowRunsResourceKey(owner, repo string, shape workflowRunsShape) string {
	key := owner + "/" + repo + "/actions/runs@" + shape.HeadSHA
	if shape.Filters != "" {
		key += "?" + shape.Filters
	}
	return key
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
