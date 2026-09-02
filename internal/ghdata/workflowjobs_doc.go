package ghdata

import (
	"encoding/json"
	"strings"
	"time"
)

// These clocks live here since both writers (fetch-on-miss and the rewriting delivery) need the same clock for a fresh answer.
// see docs/cache/rest-routes.md
const (
	// WorkflowJobsCacheTTL backstops a missed re-run delivery on a settled (otherwise immutable) answer.
	WorkflowJobsCacheTTL = 24 * time.Hour

	// WorkflowJobsQueuedTTL bounds how long a lost delivery on a waiting job goes unnoticed before a refetch.
	WorkflowJobsQueuedTTL = 60 * time.Second

	// WorkflowJobsRunningTTL is shorter: a running job's steps advance between deliveries, with no delivery reporting it.
	WorkflowJobsRunningTTL = 10 * time.Second
)

// JobsLiveness says what is moving in a stored answer, which decides how long it may be served.
type JobsLiveness int

const (
	// JobsSettled: every job has finished. Nothing can change but a re-run.
	JobsSettled JobsLiveness = iota
	// JobsQueued: nothing running yet, so a field a delivery doesn't carry is empty, not stale.
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

// StoredWorkflowStep is step of a job.
type StoredWorkflowStep struct {
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
	Number      int64   `json:"number"`
	StartedAt   *string `json:"started_at"`
	CompletedAt *string `json:"completed_at"`
}

// StoredWorkflowJob is trimmed job, as both readers serve it.
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

// StoredRunJobsPage is page of a run's jobs.
type StoredRunJobsPage struct {
	TotalCount int64               `json:"total_count"`
	Jobs       []StoredWorkflowJob `json:"jobs"`
}

// RawWorkflowJob mirrors the fields of GitHub's job object these routes model.
// The same shape arrives in a REST body and inside a `workflow_job` delivery's
// `workflow_job` key, which is what lets delivery rewrite a stored page.
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
	// JobStatusCompleted is the status whose answer has stopped moving.
	JobStatusCompleted = "completed"
	// JobStatusInProgress is the whose unreported parts move: steps advance between deliveries.
	JobStatusInProgress = "in_progress"
)

// TrimWorkflowJob converts raw job, reporting false only when the model
// cannot hold it (no id, no status). A job that is still queued or running
// trims exactly like a finished -- what its LIVENESS decides is how long
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
