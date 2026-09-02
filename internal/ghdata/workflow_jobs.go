package ghdata

import (
	"context"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Workflow jobs: GLOBAL, webhook-fed state; see docs/dashboard/dashboard.md.

// WorkflowJob is GitHub Actions job's recorded state. Empty string means
// the webhook didn't report the field.
type WorkflowJob struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	JobID        int64  `json:"job_id"`
	RunID        int64  `json:"run_id"`
	RunAttempt   int64  `json:"run_attempt"`
	Name         string `json:"name"`
	WorkflowName string `json:"workflow_name"`
	Status       string `json:"status"`     // in_progress | completed
	Conclusion   string `json:"conclusion"` // success | failure | ... (completed only)
	HeadSHA      string `json:"head_sha"`
	HeadBranch   string `json:"head_branch"`
	HTMLURL      string `json:"html_url"`
	StartedAt    string `json:"started_at"`   // RFC3339
	CompletedAt  string `json:"completed_at"` // RFC3339
	RunnerName   string `json:"runner_name"`
	UpdatedAt    string `json:"updated_at"` // RFC3339: when the last webhook was applied
}

// workflowJobRetention bounds the table's growth; see docs/dashboard/dashboard.md.
const workflowJobRetention = 14 * 24 * time.Hour

// RecordWorkflowJob upserts a job's state (out-of-order tolerant: a completed
// event never regresses to in_progress — see the UpsertWorkflowJob query) and
// prunes completed jobs whose completed_at and updated_at are both older than
// workflowJobRetention.
func (s *Store) RecordWorkflowJob(ctx context.Context, j WorkflowJob) error {
	now := time.Now().UTC()
	updatedAt := j.UpdatedAt
	if updatedAt == "" {
		updatedAt = now.Format(time.RFC3339)
	}
	completedAt := j.CompletedAt
	if j.Status == "completed" && completedAt == "" {
		// Defensive: an empty completed_at would sort as infinitely old and be swept immediately.
		completedAt = updatedAt
	}
	if err := s.q.UpsertWorkflowJob(ctx, dbgen.UpsertWorkflowJobParams{
		Owner:        j.Owner,
		Repo:         j.Repo,
		JobID:        j.JobID,
		RunID:        j.RunID,
		RunAttempt:   j.RunAttempt,
		Name:         j.Name,
		WorkflowName: j.WorkflowName,
		Status:       j.Status,
		Conclusion:   j.Conclusion,
		HeadSha:      j.HeadSHA,
		HeadBranch:   j.HeadBranch,
		HtmlUrl:      j.HTMLURL,
		StartedAt:    j.StartedAt,
		CompletedAt:  completedAt,
		RunnerName:   j.RunnerName,
		UpdatedAt:    updatedAt,
	}); err != nil {
		return err
	}
	cutoff := now.Add(-workflowJobRetention).Format(time.RFC3339)
	return s.q.PruneWorkflowJobs(ctx, dbgen.PruneWorkflowJobsParams{
		CompletedAt: cutoff,
		UpdatedAt:   cutoff,
	})
}

// RecentWorkflowJobs returns recent jobs: running (newest started),
// then completed (newest completed).
func (s *Store) RecentWorkflowJobs(ctx context.Context, limit int64) ([]WorkflowJob, error) {
	rows, err := s.q.ListRecentWorkflowJobs(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]WorkflowJob, len(rows))
	for i, r := range rows {
		out[i] = WorkflowJob{
			Owner:        r.Owner,
			Repo:         r.Repo,
			JobID:        r.JobID,
			RunID:        r.RunID,
			RunAttempt:   r.RunAttempt,
			Name:         r.Name,
			WorkflowName: r.WorkflowName,
			Status:       r.Status,
			Conclusion:   r.Conclusion,
			HeadSHA:      r.HeadSha,
			HeadBranch:   r.HeadBranch,
			HTMLURL:      r.HtmlUrl,
			StartedAt:    r.StartedAt,
			CompletedAt:  r.CompletedAt,
			RunnerName:   r.RunnerName,
			UpdatedAt:    r.UpdatedAt,
		}
	}
	return out, nil
}
