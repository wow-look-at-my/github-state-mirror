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
	"github.com/wow-look-at-my/go-containers/set"
)

// Implements the cached workflow-runs route (tier 2 of the cache contract).
// See docs/cache/rest-routes.md for the two request shapes, their invalidation
// signals, and the completeness proof that gates the repo-wide listing.

const (
	// Backstop for run deletion (never webhooked) and missed deliveries.
	workflowRunsCacheTTL = ghdata.WorkflowRunsCacheTTL

	// GitHub's default per_page for this listing.
	workflowRunsDefaultPerPage = 30

	// Caps modeled pages; deeper pagination passes through.
	workflowRunsMaxCachedPage = 10

	// Bounds the branch filter key; an unbounded key component is a cardinality footgun.
	workflowRunsMaxBranchLen = 255
)

// Closed set for the status filter (it also matches conclusion); an unknown value passes through.
var workflowRunStatuses = set.Of(
	"completed", "action_required", "cancelled",
	"failure", "neutral", "skipped", "stale",
	"success", "timed_out", "in_progress", "queued",
	"requested", "waiting", "pending",
)

// HeadSHA == "" selects the repo-wide listing (its own TTL and flush); otherwise Filters is the modeled-query key.
type workflowRunsShape struct {
	HeadSHA string
	Status  string
	Branch  string
	Filters string
	PerPage int
	Page    int
}

// listing reports the repo-wide shape, rebuilt from truth; a per-commit sha is served from a snapshot instead.
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
			if !workflowRunStatuses.Contains(v) {
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
	// Encode sorts by key, so the result does not depend on map or query order.
	shape.Filters = filters.Encode()
	return shape, true
}

// isControlRune rejects C0/DEL so a caller cannot smuggle control bytes into a cache key.
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

	// Every listed run enters truth regardless of shape, keeping listing rows warm for free.
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

// workflowRunsFromTruth rebuilds one page only behind the completeness proof -- see docs/cache/rest-routes.md.
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

// absorbWorkflowRunsIntoTruth records every listed run, and on a short page-1 answer marks the set complete -- see docs/cache/rest-routes.md.
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

// nullIfEmpty renders "not reported" as upstream's JSON null, not a fabricated empty string.
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

// workflowRunItemJSON is the trimmed workflow_runs entry consumers read.
type (
	workflowRunItemJSON = ghdata.StoredWorkflowRunItem
	workflowRunsJSON    = ghdata.StoredWorkflowRunsPage
)

// parsedWorkflowRuns is one absorbed answer: rebuild items plus truth-only fields (head_branch, run_attempt).
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
		TotalCount   *int64                   `json:"total_count"`
		WorkflowRuns *[]ghdata.RawWorkflowRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.TotalCount == nil || raw.WorkflowRuns == nil {
		return parsedWorkflowRuns{}, false
	}
	out := parsedWorkflowRuns{
		TotalCount: *raw.TotalCount,
		Runs:       make([]parsedWorkflowRun, 0, len(*raw.WorkflowRuns)),
	}
	for _, run := range *raw.WorkflowRuns {
		item, ok := ghdata.TrimWorkflowRunItem(run)
		if !ok {
			return parsedWorkflowRuns{}, false
		}
		out.Runs = append(out.Runs, parsedWorkflowRun{
			workflowRunItemJSON: item,
			HeadBranch:          derefOrEmpty(run.HeadBranch),
			RunAttempt:          run.RunAttempt,
		})
	}
	return out, true
}
