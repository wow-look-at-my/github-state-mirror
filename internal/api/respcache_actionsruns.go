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
// Two request shapes, served two different ways -- because they have two
// different invalidation signals, and the signal decides the design:
//
//   - The per-COMMIT listing (`?head_sha=<hex>`), what both mirror-pointed
//     consumers poll (survey 2026-07-11): pr-minder's hasWorkflowRuns sends
//     `?head_sha=<hex>&per_page=1` and reads ONLY `total_count` (the
//     zombie-PR probe, repeated per bot PR by the reconcile hook's fleet
//     sweep), and required-builds' listWorkflowRuns pages
//     `?head_sha=<hex>&per_page=100&page=N` reading name/status/conclusion/
//     html_url. A sha is a precise flush key -- CI deliveries name it -- so
//     this shape stays a whole-doc SNAPSHOT in workflow_runs_cache.
//
//   - The repo-wide LISTING (`?status=queued&per_page=N`, optionally
//     `branch=`), the GHA runner coordinator's queued-backlog poll. This
//     answer names no sha, so no delivery can flush it precisely -- and
//     clearing the repo's listings on every job event would be
//     invalidate-and-refetch, which the cache doctrine forbids outright. So
//     it is not a snapshot at all: it is REBUILT from the workflow_runs
//     TRUTH table, which the dispatcher maintains one run at a time
//     (onWorkflowRun applies the whole object; a workflow_job delivery
//     establishes its run's identity and raises its status floor). A run
//     entering or leaving `queued` therefore changes this answer by
//     UPDATING ONE ROW, with every other run still served.
//
// Rows alone never prove a list, so serving the listing needs a completeness
// proof: workflow_runs_list_cache records that a page-1 response for exactly
// this filter came back SHORT, which is what proves truth then held every
// matching run (the pulls_list_cache pattern). A run delivery MAINTAINS the
// rows and so must never touch that marker; only `repository` events and the
// marker's own short expiry clear it. That expiry bounds one hole -- a run
// entering the set with no delivery naming it, which needs the App
// subscribed to workflow_run since a run with no jobs yet emits no
// workflow_job -- and a queued backlog is what a coordinator provisions
// against, so it is held to minutes.
//
// Every other filter (event, actor, created, exclude_pull_requests,
// check_suite_id, ...) is unmodeled and passes through, as do repeated
// params and out-of-range paging. Deeper /actions/runs/{id}/... paths never
// reach this route (the registration is the exact literal) and keep falling
// to the NotFound passthrough.

const (
	// workflowRunsCacheTTL bounds how long a stale per-COMMIT runs page can
	// be served. CI webhooks flush within seconds of any run change; the TTL
	// is the backstop for the one signal GitHub never webhooks -- run
	// DELETION -- and for missed deliveries.
	workflowRunsCacheTTL = 24 * time.Hour

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
	Status  string
	Branch  string
	Filters string
	PerPage int
	Page    int
}

// listing reports whether this is the repo-wide shape (no head_sha). That
// shape is rebuilt from the workflow_runs truth table; a per-commit request
// is served from a snapshot instead, because a sha is a flush key and a
// backlog is not.
func (s workflowRunsShape) listing() bool { return s.HeadSHA == "" }

// filter is this request expressed as a truth-table selection.
func (s workflowRunsShape) filter() ghdata.WorkflowRunFilter {
	return ghdata.WorkflowRunFilter{Status: s.Status, HeadBranch: s.Branch, HeadSHA: s.HeadSHA}
}

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
			shape.Status = v
			filters.Set(key, v)
		case "branch":
			if v == "" || len(v) > workflowRunsMaxBranchLen || strings.ContainsFunc(v, isControlRune) {
				return workflowRunsShape{}, false
			}
			shape.Branch = v
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

// cachedWorkflowRuns serves one page of a workflow-runs listing: the
// repo-wide shape rebuilt from webhook-maintained truth rows, the per-commit
// shape from a stored whole-doc snapshot. Both fetch and absorb on a miss;
// shapes the cache does not model pass through.
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

	// HIT. The repo-wide listing is rebuilt from truth rows the dispatcher
	// maintains per run; the per-commit shape reads its snapshot.
	if shape.listing() {
		if doc, ok := h.workflowRunsFromTruth(r, owner, repo, shape, now); ok {
			h.reqlog.observe(r, DispHit)
			writeRebuilt(w, http.StatusOK, doc, true)
			return
		}
	} else if doc, ok, err := h.store.GetCachedWorkflowRuns(r.Context(), owner, repo, shape.HeadSHA, shape.PerPage, shape.Page, now); err != nil {
		slog.Warn("workflow runs cache read failed", "owner", owner, "repo", repo, "head_sha", shape.HeadSHA, "error", err)
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

	parsed, absorbed := absorbWorkflowRuns(resp.StatusCode, body)
	if overflow || !absorbed {
		// Includes 404 and 5xx: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	doc, err := renderWorkflowRuns(parsed.TotalCount, parsed.items())
	if err != nil {
		h.replayUnstored(w, r, resp, body)
		return
	}

	// Every listed run enters global truth whichever shape asked for it: a
	// run is a run, and the per-commit fetches keep the listing's rows warm
	// for free.
	h.absorbWorkflowRunsIntoTruth(r, owner, repo, shape, parsed, now)
	if !shape.listing() {
		if err := h.store.PutCachedWorkflowRuns(r.Context(), owner, repo, shape.HeadSHA, shape.PerPage, shape.Page, string(doc), now, workflowRunsCacheTTL); err != nil {
			slog.Warn("workflow runs cache write failed", "owner", owner, "repo", repo, "head_sha", shape.HeadSHA, "error", err)
		}
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, doc, false)
}

// workflowRunsFromTruth rebuilds one page of the repo-wide listing from the
// workflow_runs rows, but ONLY behind the completeness proof: rows say what
// the mirror has seen, and a list needs to know it has seen everything. With
// the marker live, the filtered count IS GitHub's total_count and offset
// pagination over the set is exact, so no page needs a separate guard.
func (h *handlers) workflowRunsFromTruth(r *http.Request, owner, repo string, shape workflowRunsShape, now time.Time) ([]byte, bool) {
	complete, err := h.store.WorkflowRunsListComplete(r.Context(), owner, repo, shape.Filters, now)
	if err != nil {
		slog.Warn("workflow runs completeness read failed", "owner", owner, "repo", repo, "filters", shape.Filters, "error", err)
		return nil, false
	}
	if !complete {
		return nil, false
	}
	rows, total, err := h.store.ListWorkflowRuns(r.Context(), owner, repo, shape.filter(), shape.PerPage, shape.Page)
	if err != nil {
		slog.Warn("workflow runs listing read failed", "owner", owner, "repo", repo, "filters", shape.Filters, "error", err)
		return nil, false
	}
	items := make([]workflowRunItemJSON, 0, len(rows))
	for _, row := range rows {
		items = append(items, workflowRunItemJSON{
			ID: row.RunID, Name: nullIfEmpty(row.Name), HeadSHA: row.HeadSHA, Status: row.Status,
			Conclusion: nullIfEmpty(row.Conclusion), HTMLURL: row.HTMLURL,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
			RunStartedAt: nullIfEmpty(row.RunStartedAt),
		})
	}
	doc, err := renderWorkflowRuns(total, items)
	if err != nil {
		slog.Warn("workflow runs listing render failed", "owner", owner, "repo", repo, "error", err)
		return nil, false
	}
	return doc, true
}

// absorbWorkflowRunsIntoTruth records every run a response listed, and -- for
// a page-1 answer SHORTER than the page size -- records the completeness
// proof that lets the listing be served from those rows afterwards. A short
// page is what proves the set was whole: it also means any row still matching
// the filter that the response omitted has moved on, so those are reconciled
// away first.
func (h *handlers) absorbWorkflowRunsIntoTruth(r *http.Request, owner, repo string, shape workflowRunsShape, parsed parsedWorkflowRuns, now time.Time) {
	ctx := r.Context()
	for _, run := range parsed.Runs {
		if err := h.store.ApplyWorkflowRun(ctx, run.truth(owner, repo), now); err != nil {
			slog.Warn("workflow run absorb failed", "owner", owner, "repo", repo, "run", run.ID, "error", err)
			return
		}
	}
	if shape.Page != 1 || len(parsed.Runs) >= shape.PerPage {
		return
	}
	kept := make([]int64, 0, len(parsed.Runs))
	for _, run := range parsed.Runs {
		kept = append(kept, run.ID)
	}
	if err := h.store.ReconcileWorkflowRuns(ctx, owner, repo, shape.filter(), kept); err != nil {
		slog.Warn("workflow runs reconcile failed", "owner", owner, "repo", repo, "filters", shape.Filters, "error", err)
		return
	}
	if err := h.store.MarkWorkflowRunsListComplete(ctx, owner, repo, shape.Filters, now, ghdata.WorkflowRunsListTTL); err != nil {
		slog.Warn("workflow runs completeness write failed", "owner", owner, "repo", repo, "filters", shape.Filters, "error", err)
	}
}

// nullIfEmpty renders a "not reported" truth column as the JSON null the
// upstream answer carried -- name, conclusion, and run_started_at are all
// nullable-but-always-keyed, and a fabricated "" would read as a real value.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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

// parsedWorkflowRuns is one absorbed /actions/runs answer: the trimmed items
// the rebuild emits, plus the run fields that go into TRUTH but never onto
// the wire (head_branch -- which the `branch` filter selects on -- and
// run_attempt).
type parsedWorkflowRuns struct {
	TotalCount int64
	Runs       []parsedWorkflowRun
}

// items is the response half: the trimmed entries, without the truth-only
// fields.
func (p parsedWorkflowRuns) items() []workflowRunItemJSON {
	out := make([]workflowRunItemJSON, 0, len(p.Runs))
	for _, run := range p.Runs {
		out = append(out, run.workflowRunItemJSON)
	}
	return out
}

type parsedWorkflowRun struct {
	workflowRunItemJSON
	HeadBranch string
	RunAttempt int64
}

// truth converts an absorbed run into its global-truth row. A REST answer
// carries the run's own status and conclusion, so it is an authoritative
// writer -- unlike a workflow_job delivery, which may only raise a floor.
func (p parsedWorkflowRun) truth(owner, repo string) ghdata.WorkflowRun {
	return ghdata.WorkflowRun{
		Owner: owner, Repo: repo, RunID: p.ID, RunAttempt: p.RunAttempt,
		Name: derefOrEmpty(p.Name), HeadSHA: p.HeadSHA, HeadBranch: p.HeadBranch,
		Status: p.Status, Conclusion: derefOrEmpty(p.Conclusion), HTMLURL: p.HTMLURL,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		RunStartedAt: derefOrEmpty(p.RunStartedAt),
	}
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// renderWorkflowRuns marshals the trimmed runs document. Both sources go
// through it -- an absorbed upstream answer and a rebuild from truth rows --
// so a hit and a miss cannot drift apart in shape.
func renderWorkflowRuns(total int64, items []workflowRunItemJSON) ([]byte, error) {
	if items == nil {
		items = []workflowRunItemJSON{}
	}
	return marshalTrimmed(workflowRunsJSON{TotalCount: total, WorkflowRuns: items})
}

// absorbWorkflowRuns parses an /actions/runs 200. Reports false -- serve
// verbatim, store nothing -- for any other status or any shape the model
// cannot hold: total_count and the workflow_runs array must both be PRESENT,
// and every run must carry a positive id, a status, and a full-hex head sha.
// An empty workflow_runs with total_count 0 is a valid, cacheable answer
// (exactly the "no runs yet" verdict the zombie probe is after).
func absorbWorkflowRuns(status int, body []byte) (parsedWorkflowRuns, bool) {
	if status != http.StatusOK {
		return parsedWorkflowRuns{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return parsedWorkflowRuns{}, false
	}
	var raw struct {
		TotalCount   *int64 `json:"total_count"`
		WorkflowRuns *[]struct {
			ID           int64   `json:"id"`
			RunAttempt   int64   `json:"run_attempt"`
			Name         *string `json:"name"`
			HeadSHA      string  `json:"head_sha"`
			HeadBranch   *string `json:"head_branch"`
			Status       string  `json:"status"`
			Conclusion   *string `json:"conclusion"`
			HTMLURL      string  `json:"html_url"`
			CreatedAt    string  `json:"created_at"`
			UpdatedAt    string  `json:"updated_at"`
			RunStartedAt *string `json:"run_started_at"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.TotalCount == nil || raw.WorkflowRuns == nil {
		return parsedWorkflowRuns{}, false
	}
	out := parsedWorkflowRuns{
		TotalCount: *raw.TotalCount,
		Runs:       make([]parsedWorkflowRun, 0, len(*raw.WorkflowRuns)),
	}
	for _, run := range *raw.WorkflowRuns {
		sha := strings.ToLower(run.HeadSHA)
		if run.ID <= 0 || run.Status == "" || !isFullHexSHA(sha) {
			return parsedWorkflowRuns{}, false
		}
		out.Runs = append(out.Runs, parsedWorkflowRun{
			workflowRunItemJSON: workflowRunItemJSON{
				ID: run.ID, Name: run.Name, HeadSHA: sha, Status: run.Status,
				Conclusion: run.Conclusion, HTMLURL: run.HTMLURL,
				CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, RunStartedAt: run.RunStartedAt,
			},
			HeadBranch: derefOrEmpty(run.HeadBranch),
			RunAttempt: run.RunAttempt,
		})
	}
	return out, true
}
