package ghdata

import (
	"context"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Workflow jobs.
//
// GitHub Actions job state fed by workflow_job webhooks. Like the webhook
// delivery log, this data is GLOBAL (not actor-scoped): it is webhook-fed
// operational telemetry with no per-credential fetch path — a job's state only
// ever arrives via the HMAC-verified delivery, never through a caller's token,
// so there is no credential to partition by. The read path (GET /api/jobs) is
// admin-only, consistent with the other global logs.

// WorkflowJob is one GitHub Actions job's recorded state. Empty string means
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

// workflowJobRetention bounds the table's growth: completed jobs older than
// this on BOTH clocks — the job's own completed_at AND the row's updated_at
// (when the last webhook was applied) — are pruned after each upsert (one
// cheap indexed DELETE — see PruneWorkflowJobs). The updated_at key is what
// keeps the upsert's out-of-order guard sound: the row is the guard's only
// memory, so it must survive a full retention window after the last delivery
// touched it, not merely after the event's own timestamp (a replayed old
// completed event would otherwise be recorded and pruned by the same call,
// and a late in_progress would then resurrect the job as a phantom running
// row). Jobs still in progress are never pruned. Two weeks keeps enough
// history to be useful while a CI-heavy org's job volume stays bounded; no
// config knob on purpose (this is observability, not source-of-truth).
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
		// Defensive: a completed event always carries completed_at, but the prune
		// compares completed_at lexicographically, and '' would read as infinitely
		// old and be swept immediately.
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

// RecentWorkflowJobs returns recent jobs: running first (newest started first),
// then completed (newest completed first).
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
