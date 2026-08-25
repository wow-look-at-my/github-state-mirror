package webhook

import (
	"encoding/json"
	"fmt"
)

// GitHub Actions payload parsing: workflow_job (per-JOB state) and
// workflow_run (per-RUN state). Both feed global truth -- the workflow_jobs
// table and the workflow_runs table the repo-wide runs listing is rebuilt
// from -- so both parse strictly: a payload with no identifiable job or run
// is an error, never a guess.

// WorkflowJobPayload is a GitHub Actions job's state parsed from a workflow_job webhook; empty string means the field was not reported yet.
// see docs/webhooks/response-cache-invalidation.md
type WorkflowJobPayload struct {
	Owner        string
	Repo         string
	JobID        int64
	RunID        int64
	RunAttempt   int64
	Name         string
	WorkflowName string
	Status       string // in_progress | completed
	Conclusion   string // success | failure | cancelled | ... (completed only)
	HeadSHA      string
	HeadBranch   string
	HTMLURL      string
	StartedAt    string // RFC3339
	CompletedAt  string // RFC3339
	RunnerName   string
}

// ParseWorkflowJobPayload extracts a job's state from a workflow_job webhook.
func ParseWorkflowJobPayload(raw json.RawMessage) (WorkflowJobPayload, error) {
	var body struct {
		WorkflowJob *struct {
			ID           int64   `json:"id"`
			RunID        int64   `json:"run_id"`
			RunAttempt   int64   `json:"run_attempt"`
			Name         string  `json:"name"`
			WorkflowName *string `json:"workflow_name"`
			Status       string  `json:"status"`
			Conclusion   *string `json:"conclusion"`
			HeadSHA      string  `json:"head_sha"`
			HeadBranch   *string `json:"head_branch"`
			HTMLURL      string  `json:"html_url"`
			StartedAt    *string `json:"started_at"`
			CompletedAt  *string `json:"completed_at"`
			RunnerName   *string `json:"runner_name"`
		} `json:"workflow_job"`
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return WorkflowJobPayload{}, fmt.Errorf("parse workflow_job payload: %w", err)
	}
	if body.WorkflowJob == nil || body.Repository == nil {
		return WorkflowJobPayload{}, fmt.Errorf("parse workflow_job payload: missing workflow_job/repository")
	}
	j := body.WorkflowJob
	p := WorkflowJobPayload{
		Owner:        body.Repository.Owner.Login,
		Repo:         body.Repository.Name,
		JobID:        j.ID,
		RunID:        j.RunID,
		RunAttempt:   j.RunAttempt,
		Name:         j.Name,
		WorkflowName: strOrEmpty(j.WorkflowName),
		Status:       j.Status,
		Conclusion:   strOrEmpty(j.Conclusion),
		HeadSHA:      j.HeadSHA,
		HeadBranch:   strOrEmpty(j.HeadBranch),
		HTMLURL:      j.HTMLURL,
		RunnerName:   strOrEmpty(j.RunnerName),
	}
	if ts := strOrEmpty(j.StartedAt); ts != "" {
		p.StartedAt = normaliseTime(ts)
	}
	if ts := strOrEmpty(j.CompletedAt); ts != "" {
		p.CompletedAt = normaliseTime(ts)
	}
	if p.Owner == "" || p.Repo == "" || p.JobID == 0 {
		return WorkflowJobPayload{}, fmt.Errorf("parse workflow_job payload: missing owner/repo/job id")
	}
	return p, nil
}

// ParseWorkflowRunHeadSHA extracts workflow_run.head_sha ("" when absent); the dispatcher's only use is flushing that sha's cached runs pages.
func ParseWorkflowRunHeadSHA(raw json.RawMessage) string {
	sha, _ := ParseWorkflowRunIdentity(raw)
	return sha
}

// ParseWorkflowRunIdentity extracts a workflow_run payload's head_sha and run
// id (zero values when absent or unparseable). The run id is what flushes the
// run's cached JOB answers: a re-run replaces a run's jobs under the SAME id,
// and the workflow_run delivery is the signal that happened.
func ParseWorkflowRunIdentity(raw json.RawMessage) (headSHA string, runID int64) {
	var body struct {
		WorkflowRun *struct {
			ID      int64  `json:"id"`
			HeadSHA string `json:"head_sha"`
		} `json:"workflow_run"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.WorkflowRun == nil {
		return "", 0
	}
	return body.WorkflowRun.HeadSHA, body.WorkflowRun.ID
}

// WorkflowRunPayload is an Actions run's state parsed from a workflow_run
// webhook -- the authoritative per-run signal, and the only one for a run
// that creates no jobs at all (a startup_failure, or a run held by a
// concurrency group). Empty string means the payload did not report the
// field (Conclusion until completed, RunStartedAt until it starts).
type WorkflowRunPayload struct {
	Owner        string
	Repo         string
	RunID        int64
	RunAttempt   int64
	Name         string
	HeadSHA      string
	HeadBranch   string
	Status       string
	Conclusion   string
	HTMLURL      string
	CreatedAt    string
	UpdatedAt    string
	RunStartedAt string
}

// ParseWorkflowRunPayload extracts a run's whole state from a workflow_run
// webhook. A payload missing the run or repository object is an error: the
// dispatcher applies this to global truth, and a run with no identity is not
// something to guess at.
func ParseWorkflowRunPayload(raw json.RawMessage) (WorkflowRunPayload, error) {
	var body struct {
		WorkflowRun *struct {
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
		} `json:"workflow_run"`
		Repository *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return WorkflowRunPayload{}, fmt.Errorf("parse workflow_run payload: %w", err)
	}
	if body.WorkflowRun == nil || body.Repository == nil {
		return WorkflowRunPayload{}, fmt.Errorf("parse workflow_run payload: missing workflow_run/repository")
	}
	r := body.WorkflowRun
	if r.ID <= 0 || r.Status == "" {
		return WorkflowRunPayload{}, fmt.Errorf("parse workflow_run payload: run has no id or status")
	}
	return WorkflowRunPayload{
		Owner:        body.Repository.Owner.Login,
		Repo:         body.Repository.Name,
		RunID:        r.ID,
		RunAttempt:   r.RunAttempt,
		Name:         strOrEmpty(r.Name),
		HeadSHA:      r.HeadSHA,
		HeadBranch:   strOrEmpty(r.HeadBranch),
		Status:       r.Status,
		Conclusion:   strOrEmpty(r.Conclusion),
		HTMLURL:      r.HTMLURL,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
		RunStartedAt: strOrEmpty(r.RunStartedAt),
	}, nil
}
