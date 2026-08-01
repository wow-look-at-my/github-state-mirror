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

// The cached Actions JOB reads (tier 2 of the cache contract):
//
//	GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs
//	GET /repos/{owner}/{repo}/actions/jobs/{job_id}
//
// Together the second- and fourth-largest unrouted slices of the request log.
//
// ONLY TERMINAL ANSWERS ARE STORED: a jobs page whose every job has
// `completed`, or a single completed job. A queued/in_progress job is a LIVE
// value the GHA runner coordinator provisions against — serving one even
// seconds stale could over- or under-provision runners — and no TTL short
// enough to be safe there would be long enough to be worth having. Those
// always reach GitHub, recorded (like every modeled-request/unmodeled-response
// case) as a passthrough. What that leaves is the fleet re-reading SETTLED
// runs forever, which is the traffic actually worth killing, and it is the
// same stance the closed-PR cache takes: cache what has stopped moving.
//
// Even a finished run can change: a RE-RUN replaces its jobs under the same
// run id. Both kinds therefore carry the owning run_id, and workflow_job /
// workflow_run deliveries flush every row under that run.

const (
	// workflowJobsCacheTTL backstops a missed re-run delivery. A completed
	// job is otherwise immutable, so this is long.
	workflowJobsCacheTTL = 24 * time.Hour

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

// cachedRunJobs serves one page of a run's jobs, absorbing only once every
// job on the page has completed.
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

// cachedWorkflowJob serves one job, absorbing only once it has completed.
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
	// absorb renders the trimmed document and reports the owning run id.
	// ok=false means "not terminal, or not a shape we hold": relay unstored.
	absorb func(status int, body []byte) (doc string, runID int64, ok bool)
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

	doc, runID, absorbed := rt.absorb(resp.StatusCode, body)
	if overflow || !absorbed {
		// Not terminal yet (the common live case), a 404, or a shape the
		// model cannot hold: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedWorkflowJobs(r.Context(), rt.owner, rt.repo, rt.kind, rt.refID, runID, rt.perPage, rt.page, doc, now, workflowJobsCacheTTL); err != nil {
		slog.Warn("workflow jobs cache write failed", "owner", rt.owner, "repo", rt.repo, "kind", rt.kind, "id", rt.refID, "error", err)
	}
	h.refreshGrantOn2xx(r, rt.owner, rt.repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// ---- absorb ----

// workflowJobJSON is the trimmed job. Every URL field is dropped (no consumer
// survey has pinned one here; the brief's Callers line names who to survey if
// one is ever needed). `conclusion` is nullable-but-always-keyed, like every
// other rebuilt nullable in this package.
type workflowJobJSON struct {
	ID           int64              `json:"id"`
	RunID        int64              `json:"run_id"`
	RunAttempt   int64              `json:"run_attempt"`
	WorkflowName *string            `json:"workflow_name"`
	HeadBranch   *string            `json:"head_branch"`
	HeadSHA      string             `json:"head_sha"`
	Name         string             `json:"name"`
	Status       string             `json:"status"`
	Conclusion   *string            `json:"conclusion"`
	CreatedAt    *string            `json:"created_at"`
	StartedAt    *string            `json:"started_at"`
	CompletedAt  *string            `json:"completed_at"`
	Labels       []string           `json:"labels"`
	RunnerName   *string            `json:"runner_name"`
	Steps        []workflowStepJSON `json:"steps,omitempty"`
}

type workflowStepJSON struct {
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
	Number      int64   `json:"number"`
	StartedAt   *string `json:"started_at"`
	CompletedAt *string `json:"completed_at"`
}

type runJobsJSON struct {
	TotalCount int64             `json:"total_count"`
	Jobs       []workflowJobJSON `json:"jobs"`
}

// rawJob mirrors the fields of GitHub's job object this route models.
type rawJob struct {
	ID           int64    `json:"id"`
	RunID        int64    `json:"run_id"`
	RunAttempt   int64    `json:"run_attempt"`
	WorkflowName *string  `json:"workflow_name"`
	HeadBranch   *string  `json:"head_branch"`
	HeadSHA      string   `json:"head_sha"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Conclusion   *string  `json:"conclusion"`
	CreatedAt    *string  `json:"created_at"`
	StartedAt    *string  `json:"started_at"`
	CompletedAt  *string  `json:"completed_at"`
	Labels       []string `json:"labels"`
	RunnerName   *string  `json:"runner_name"`
	Steps        []struct {
		Name        string  `json:"name"`
		Status      string  `json:"status"`
		Conclusion  *string `json:"conclusion"`
		Number      int64   `json:"number"`
		StartedAt   *string `json:"started_at"`
		CompletedAt *string `json:"completed_at"`
	} `json:"steps"`
}

// jobStatusCompleted is the one status whose answer has stopped moving.
const jobStatusCompleted = "completed"

// trimJob converts one raw job, reporting false when the model cannot hold it
// (no id, no status) or when it is not terminal.
func trimJob(j rawJob) (workflowJobJSON, bool) {
	if j.ID <= 0 || j.Status == "" || !strings.EqualFold(j.Status, jobStatusCompleted) {
		return workflowJobJSON{}, false
	}
	out := workflowJobJSON{
		ID: j.ID, RunID: j.RunID, RunAttempt: j.RunAttempt,
		WorkflowName: j.WorkflowName, HeadBranch: j.HeadBranch,
		HeadSHA: strings.ToLower(j.HeadSHA), Name: j.Name,
		Status: j.Status, Conclusion: j.Conclusion,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, CompletedAt: j.CompletedAt,
		Labels: j.Labels, RunnerName: j.RunnerName,
	}
	if out.Labels == nil {
		out.Labels = []string{}
	}
	for _, s := range j.Steps {
		out.Steps = append(out.Steps, workflowStepJSON{
			Name: s.Name, Status: s.Status, Conclusion: s.Conclusion,
			Number: s.Number, StartedAt: s.StartedAt, CompletedAt: s.CompletedAt,
		})
	}
	return out, true
}

// absorbRunJobs renders a run's jobs page. It absorbs only a 200 whose EVERY
// job has completed: one in-flight job makes the whole page a live answer.
// An empty page (past the end) is a valid cacheable answer, but it carries no
// run id — so it is deliberately NOT stored, since a row nothing can flush is
// a row that outlives its truth.
func absorbRunJobs(status int, body []byte) (string, int64, bool) {
	if status != http.StatusOK {
		return "", 0, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", 0, false
	}
	var raw struct {
		TotalCount *int64    `json:"total_count"`
		Jobs       *[]rawJob `json:"jobs"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.TotalCount == nil || raw.Jobs == nil {
		return "", 0, false
	}
	out := runJobsJSON{TotalCount: *raw.TotalCount, Jobs: []workflowJobJSON{}}
	runID := int64(0)
	for _, j := range *raw.Jobs {
		tj, ok := trimJob(j)
		if !ok {
			return "", 0, false
		}
		if tj.RunID > 0 {
			runID = tj.RunID
		}
		out.Jobs = append(out.Jobs, tj)
	}
	if runID <= 0 {
		return "", 0, false
	}
	rendered, err := marshalTrimmed(out)
	if err != nil {
		return "", 0, false
	}
	return string(rendered), runID, true
}

// absorbSingleJob renders one job, absorbing only a completed one.
func absorbSingleJob(status int, body []byte) (string, int64, bool) {
	if status != http.StatusOK {
		return "", 0, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", 0, false
	}
	var raw rawJob
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return "", 0, false
	}
	job, ok := trimJob(raw)
	if !ok || job.RunID <= 0 {
		return "", 0, false
	}
	rendered, err := marshalTrimmed(job)
	if err != nil {
		return "", 0, false
	}
	return string(rendered), job.RunID, true
}
