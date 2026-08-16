package ghdata

import (
	"encoding/json"
	"strings"
	"time"
)

// The three clocks a stored job answer can carry. They live here because BOTH
// writers need them: the fetch-on-miss path and the delivery that rewrites a
// row. A rewritten answer is exactly as fresh as a fetched one, so it gets the
// same clock (the BranchesCacheTTL precedent).
const (
	// WorkflowJobsCacheTTL backstops a missed re-run delivery on a SETTLED
	// answer. A completed job is otherwise immutable, so this is long.
	WorkflowJobsCacheTTL = 24 * time.Hour

	// WorkflowJobsQueuedTTL bounds an answer whose only movement is jobs
	// WAITING to start. Deliveries maintain such a row, so this is not "how
	// stale we tolerate" -- it is how long a LOST delivery can go unnoticed
	// before the next read re-fetches. The GHA coordinator provisions runners
	// against these answers, which is what keeps it short.
	WorkflowJobsQueuedTTL = 60 * time.Second

	// WorkflowJobsRunningTTL bounds an answer with a job actually RUNNING,
	// and it is shorter for a reason deliveries cannot fix: GitHub sends one
	// delivery per job TRANSITION (queued, in_progress, completed), while a
	// running job's `steps` advance between them with no delivery at all. So
	// a rewritten entry is exactly right about a queued job -- whose steps
	// are empty by construction -- and knowably behind on a running one's
	// steps. This is the bound on that, and the only part of the answer no
	// webhook can keep current.
	WorkflowJobsRunningTTL = 10 * time.Second
)

// JobsLiveness says what is moving in a stored answer, which is what decides
// how long it may be served.
type JobsLiveness int

const (
	// JobsSettled: every job has finished. Nothing can change but a re-run.
	JobsSettled JobsLiveness = iota
	// JobsQueued: something is still to come, but nothing is running -- so
	// every field a delivery does not carry is empty rather than stale.
	JobsQueued
	// JobsRunning: a job is in progress, and its steps advance unreported.
	JobsRunning
)

// TTL is how long an answer of this liveness may be served without a fetch.
func (l JobsLiveness) TTL() time.Duration {
	switch l {
	case JobsRunning:
		return WorkflowJobsRunningTTL
	case JobsQueued:
		return WorkflowJobsQueuedTTL
	default:
		return WorkflowJobsCacheTTL
	}
}

// LivenessOf reports the strongest movement among a set of jobs.
func LivenessOf(jobs ...StoredWorkflowJob) JobsLiveness {
	out := JobsSettled
	for _, j := range jobs {
		switch {
		case JobIsTerminal(j):
		case strings.EqualFold(j.Status, JobStatusInProgress):
			return JobsRunning
		default:
			out = JobsQueued
		}
	}
	return out
}

// The stored shape of the Actions JOB reads:
//
//	GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs
//	GET /repos/{owner}/{repo}/actions/jobs/{job_id}
//
// It lives here, not in the API layer, because TWO writers render it and both
// must produce the same bytes for the same job: the fetch-on-miss path, and
// the `workflow_job` delivery that rewrites a job's entry inside a stored page
// (ApplyWorkflowJob). That is also why the TRIM is here rather than duplicated
// -- GitHub's job object arrives on both paths, from a REST body and from a
// delivery, and there must be exactly one answer for what it becomes.
//
// Field order is wire order. Every URL field is dropped (no consumer survey
// has pinned one here); `conclusion` and the other nullables are
// nullable-but-always-keyed, like every rebuilt nullable in the response
// caches.

// StoredWorkflowStep is one step of a job.
type StoredWorkflowStep struct {
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
	Number      int64   `json:"number"`
	StartedAt   *string `json:"started_at"`
	CompletedAt *string `json:"completed_at"`
}

// StoredWorkflowJob is one trimmed job, as both readers serve it.
type StoredWorkflowJob struct {
	ID           int64                `json:"id"`
	RunID        int64                `json:"run_id"`
	RunAttempt   int64                `json:"run_attempt"`
	WorkflowName *string              `json:"workflow_name"`
	HeadBranch   *string              `json:"head_branch"`
	HeadSHA      string               `json:"head_sha"`
	Name         string               `json:"name"`
	Status       string               `json:"status"`
	Conclusion   *string              `json:"conclusion"`
	CreatedAt    *string              `json:"created_at"`
	StartedAt    *string              `json:"started_at"`
	CompletedAt  *string              `json:"completed_at"`
	Labels       []string             `json:"labels"`
	RunnerName   *string              `json:"runner_name"`
	Steps        []StoredWorkflowStep `json:"steps,omitempty"`
}

// StoredRunJobsPage is one page of a run's jobs.
type StoredRunJobsPage struct {
	TotalCount int64               `json:"total_count"`
	Jobs       []StoredWorkflowJob `json:"jobs"`
}

// RawWorkflowJob mirrors the fields of GitHub's job object these routes model.
// The same shape arrives in a REST body and inside a `workflow_job` delivery's
// `workflow_job` key, which is what lets one delivery rewrite a stored page.
type RawWorkflowJob struct {
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

const (
	// JobStatusCompleted is the one status whose answer has stopped moving.
	JobStatusCompleted = "completed"
	// JobStatusInProgress is the one whose UNREPORTED parts move: steps
	// advance between deliveries.
	JobStatusInProgress = "in_progress"
)

// TrimWorkflowJob converts one raw job, reporting false only when the model
// cannot hold it (no id, no status). A job that is still queued or running
// trims exactly like a finished one -- what its LIVENESS decides is how long
// the row may be served without a delivery (WorkflowJobsLiveTTL), not whether
// it can be represented.
func TrimWorkflowJob(j RawWorkflowJob) (StoredWorkflowJob, bool) {
	if j.ID <= 0 || j.Status == "" {
		return StoredWorkflowJob{}, false
	}
	out := StoredWorkflowJob{
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
		out.Steps = append(out.Steps, StoredWorkflowStep{
			Name: s.Name, Status: s.Status, Conclusion: s.Conclusion,
			Number: s.Number, StartedAt: s.StartedAt, CompletedAt: s.CompletedAt,
		})
	}
	return out, true
}

// JobIsTerminal reports whether a trimmed job has stopped moving.
func JobIsTerminal(j StoredWorkflowJob) bool {
	return strings.EqualFold(j.Status, JobStatusCompleted)
}

// TrimWorkflowJobJSON trims GitHub's job object straight from its JSON, which
// is how a `workflow_job` delivery reaches the rewrite.
func TrimWorkflowJobJSON(raw json.RawMessage) (StoredWorkflowJob, bool) {
	var j RawWorkflowJob
	if err := json.Unmarshal(raw, &j); err != nil {
		return StoredWorkflowJob{}, false
	}
	return TrimWorkflowJob(j)
}
