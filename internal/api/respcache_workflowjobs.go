package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The cached Actions JOB reads (tier 2 of the cache contract):
//
//	GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs
//	GET /repos/{owner}/{repo}/actions/jobs/{job_id}
//
// Together the second- and fourth-largest unrouted slices of the request log.
//
// A JOB STILL MOVING IS STORED TOO, AND KEPT FRESH BY ITS OWN DELIVERIES.
// These rows used to hold terminal answers only, because a queued/in_progress
// job is a live value the GHA runner coordinator provisions against and no TTL
// short enough to be safe would be long enough to be worth having. That is
// still true OF A TTL — and it is the wrong instrument. `workflow_job`
// deliveries carry the whole job object, so a stored answer is REWRITTEN as
// the job moves (ghdata.ApplyWorkflowJob) rather than aged: the fetch is what
// proves a page's membership, and the deliveries are what keep its contents
// current. What the short live TTL bounds is a LOST delivery, nothing else.
//
// The alternative was leaving three quarters of the mirror's passthrough
// volume permanently uncacheable, with the fleet re-asking GitHub for state it
// had already been told.
//
// Even a finished run can change: a RE-RUN replaces its jobs under the same
// run id. Both kinds therefore carry the owning run_id, and a `workflow_run`
// delivery — plus any job delivery the stored page cannot absorb — flushes
// every row under that run.

const (
	// The two TTLs live in the store: a delivery rewrites these rows and must
	// date them exactly as a fetch would.
	workflowJobsCacheTTL = ghdata.WorkflowJobsCacheTTL
	workflowJobsLiveTTL  = ghdata.WorkflowJobsLiveTTL

	workflowJobsDefaultPerPage = 30
	// workflowJobsMaxCachedPage caps the modeled pages; deeper pagination
	// passes through. A run with more than 30 pages of jobs at the default
	// page size does not exist in this fleet.
	workflowJobsMaxCachedPage = 10
)

// parseWorkflowJobsShape reports the paging shape of a run-jobs query and
// whether the cache models it. `filter` (latest/all) selects a DIFFERENT set
// of jobs for a re-run and is deliberately unmodeled, as is anything else.
func parseWorkflowJobsShape(q url.Values) (perPage, page int, ok bool) {
	perPage, page = workflowJobsDefaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return 0, 0, false
		}
		switch key {
		case "per_page":
			n, err := strconv.Atoi(vals[0])
			if err != nil || n < 1 || n > 100 {
				return 0, 0, false
			}
			perPage = n
		case "page":
			n, err := strconv.Atoi(vals[0])
			if err != nil || n < 1 || n > workflowJobsMaxCachedPage {
				return 0, 0, false
			}
			page = n
		default:
			return 0, 0, false
		}
	}
	return perPage, page, true
}

// cachedRunJobs serves one page of a run's jobs.
func (h *handlers) cachedRunJobs(w http.ResponseWriter, r *http.Request) {
	owner, repo := chi.URLParam(r, "owner"), chi.URLParam(r, "repo")
	runID, err := strconv.ParseInt(chi.URLParam(r, "run_id"), 10, 64)
	if err != nil || runID <= 0 {
		h.passthrough(w, r, PassPath)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	perPage, page, ok := parseWorkflowJobsShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	h.serveWorkflowJobs(w, r, workflowJobsRoute{
		owner: owner, repo: repo, kind: ghdata.WorkflowJobsKindRunJobs,
		refID: runID, perPage: int64(perPage), page: int64(page),
		denyKind: denyKindRunJobs,
		absorb:   absorbRunJobs,
	})
}

// cachedWorkflowJob serves one job.
func (h *handlers) cachedWorkflowJob(w http.ResponseWriter, r *http.Request) {
	owner, repo := chi.URLParam(r, "owner"), chi.URLParam(r, "repo")
	jobID, err := strconv.ParseInt(chi.URLParam(r, "job_id"), 10, 64)
	if err != nil || jobID <= 0 {
		h.passthrough(w, r, PassPath)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	if len(r.URL.Query()) > 0 {
		h.passthrough(w, r, PassQuery)
		return
	}
	h.serveWorkflowJobs(w, r, workflowJobsRoute{
		owner: owner, repo: repo, kind: ghdata.WorkflowJobsKindJob,
		refID: jobID, perPage: 1, page: 1,
		denyKind: denyKindWorkflowJob,
		absorb:   absorbSingleJob,
	})
}

// workflowJobsRoute is the per-route configuration the shared flow needs. The
// two routes differ only in their key, their deny kind, and how a response is
// absorbed — everything else (reveal, hit, fetch, store, serve) is identical,
// so it lives once.
type workflowJobsRoute struct {
	owner, repo string
	kind        string
	refID       int64
	perPage     int64
	page        int64
	denyKind    string
	// absorb renders the trimmed document. ok=false means "not a shape we
	// hold": relay unstored.
	absorb func(status int, body []byte) (absorbedJobs, bool)
}

// absorbedJobs is one rendered job answer plus what decides how long it may be
// served: the owning run, and whether anything in it is still moving.
type absorbedJobs struct {
	doc   string
	runID int64
	live  bool
}

// ttl is the live TTL for an answer with a job still moving and the long one
// for a settled answer. A live row is not a stale row waiting to expire: it is
// maintained by `workflow_job` deliveries, which rewrite the job's entry in
// place. The short TTL is what BOUNDS a lost delivery, and nothing else.
func (a absorbedJobs) ttl() time.Duration {
	if a.live {
		return workflowJobsLiveTTL
	}
	return workflowJobsCacheTTL
}

func (h *handlers) serveWorkflowJobs(w http.ResponseWriter, r *http.Request, rt workflowJobsRoute) {
	resourceKey := ghdata.NormalizeRepoKey(rt.owner) + "/" + ghdata.NormalizeRepoKey(rt.repo) +
		"/" + rt.kind + "/" + strconv.FormatInt(rt.refID, 10)
	switch outcome, verdict, cached := h.reveal(r, rt.owner, rt.repo, rt.denyKind, resourceKey); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if doc, ok, err := h.store.GetCachedWorkflowJobs(r.Context(), rt.owner, rt.repo, rt.kind, rt.refID, rt.perPage, rt.page, now); err != nil {
		slog.Warn("workflow jobs cache read failed", "owner", rt.owner, "repo", rt.repo, "kind", rt.kind, "id", rt.refID, "error", err)
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

	got, absorbed := rt.absorb(resp.StatusCode, body)
	if overflow || !absorbed {
		// A 404, or a shape the model cannot hold: relayed verbatim, never
		// stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedWorkflowJobs(r.Context(), rt.owner, rt.repo, rt.kind, rt.refID, got.runID, rt.perPage, rt.page, got.doc, now, got.ttl()); err != nil {
		slog.Warn("workflow jobs cache write failed", "owner", rt.owner, "repo", rt.repo, "kind", rt.kind, "id", rt.refID, "error", err)
	}
	h.refreshGrantOn2xx(r, rt.owner, rt.repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(got.doc), false)
}

// ---- absorb ----

// The trimmed shapes live in the store: a `workflow_job` delivery rewrites a
// job's entry inside a stored page, so the render and the rewrite must agree
// byte for byte and there is exactly one definition of each.
type (
	workflowJobJSON = ghdata.StoredWorkflowJob
	runJobsJSON     = ghdata.StoredRunJobsPage
)

type rawJob = ghdata.RawWorkflowJob

// absorbRunJobs renders a run's jobs page. A page with a job still moving is
// absorbed too, marked live: the fetch is what proves the page's MEMBERSHIP,
// and the `workflow_job` deliveries that follow rewrite each job's entry in
// place, so the stored answer tracks the run instead of expiring against it.
//
// An empty page (past the end) is a valid cacheable answer, but it carries no
// run id — so it is deliberately NOT stored, since a row nothing can flush is
// a row that outlives its truth.
func absorbRunJobs(status int, body []byte) (absorbedJobs, bool) {
	if status != http.StatusOK {
		return absorbedJobs{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return absorbedJobs{}, false
	}
	var raw struct {
		TotalCount *int64    `json:"total_count"`
		Jobs       *[]rawJob `json:"jobs"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.TotalCount == nil || raw.Jobs == nil {
		return absorbedJobs{}, false
	}
	out := runJobsJSON{TotalCount: *raw.TotalCount, Jobs: []workflowJobJSON{}}
	got := absorbedJobs{}
	for _, j := range *raw.Jobs {
		tj, ok := ghdata.TrimWorkflowJob(j)
		if !ok {
			return absorbedJobs{}, false
		}
		if tj.RunID > 0 {
			got.runID = tj.RunID
		}
		if !ghdata.JobIsTerminal(tj) {
			got.live = true
		}
		out.Jobs = append(out.Jobs, tj)
	}
	if got.runID <= 0 {
		return absorbedJobs{}, false
	}
	rendered, err := marshalTrimmed(out)
	if err != nil {
		return absorbedJobs{}, false
	}
	got.doc = string(rendered)
	return got, true
}

// absorbSingleJob renders one job. A job still moving is absorbed too, marked
// live: its own deliveries rewrite the row.
func absorbSingleJob(status int, body []byte) (absorbedJobs, bool) {
	if status != http.StatusOK {
		return absorbedJobs{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return absorbedJobs{}, false
	}
	var raw rawJob
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return absorbedJobs{}, false
	}
	job, ok := ghdata.TrimWorkflowJob(raw)
	if !ok || job.RunID <= 0 {
		return absorbedJobs{}, false
	}
	rendered, err := marshalTrimmed(job)
	if err != nil {
		return absorbedJobs{}, false
	}
	return absorbedJobs{doc: string(rendered), runID: job.RunID, live: !ghdata.JobIsTerminal(job)}, true
}
