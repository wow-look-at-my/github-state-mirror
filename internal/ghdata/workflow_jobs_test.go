package ghdata

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ts(t *testing.T, ago time.Duration) string {
	t.Helper()
	return time.Now().UTC().Add(-ago).Format(time.RFC3339)
}

// TestRecordWorkflowJob_PrunesOldCompleted verifies the on-write prune: a
// completed job stale on both clocks (completed_at AND updated_at past the
// retention window) is deleted by the next write, while a fresh completed job
// and an old-but-still-running job survive. Rows whose updated_at is fresh
// are NOT pruned however old their completed_at is -- that case is pinned by
// TestRecordWorkflowJob_LateInProgressPastRetentionDoesNotRegress.
func TestRecordWorkflowJob_PrunesOldCompleted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A completed job well past the 14-day retention, last touched just as
	// long ago (the explicit UpdatedAt models a row recorded 15 days back).
	require.NoError(t, s.RecordWorkflowJob(ctx, WorkflowJob{
		Owner: "o", Repo: "r", JobID: 1, Name: "ancient", Status: "completed",
		Conclusion: "success", StartedAt: ts(t, 16*24*time.Hour), CompletedAt: ts(t, 15*24*time.Hour),
		UpdatedAt: ts(t, 15*24*time.Hour),
	}))
	// A running job that is just as old — must never be pruned.
	require.NoError(t, s.RecordWorkflowJob(ctx, WorkflowJob{
		Owner: "o", Repo: "r", JobID: 2, Name: "stuck", Status: "in_progress",
		StartedAt: ts(t, 15*24*time.Hour),
	}))
	// A fresh write triggers the prune.
	require.NoError(t, s.RecordWorkflowJob(ctx, WorkflowJob{
		Owner: "o", Repo: "r", JobID: 3, Name: "fresh", Status: "completed",
		Conclusion: "success", StartedAt: ts(t, 10*time.Minute), CompletedAt: ts(t, 5*time.Minute),
	}))

	jobs, err := s.RecentWorkflowJobs(ctx, 10)
	require.NoError(t, err)
	names := make([]string, len(jobs))
	for i, j := range jobs {
		names[i] = j.Name
	}
	assert.ElementsMatch(t, []string{"stuck", "fresh"}, names,
		"the >14d completed job must be pruned; the old running job must survive")
}

// TestRecordWorkflowJob_LateInProgressPastRetentionDoesNotRegress pins the
// out-of-order guarantee against the retention prune (the 2026-07-17 CI
// incident): a completed event whose completed_at is already past the 14-day
// horizon (a replayed/redelivered old delivery) is still fresh telemetry --
// the on-write prune keys on updated_at too, so it cannot delete the row it
// just wrote -- and a late in_progress for that job is absorbed by the upsert
// guard instead of INSERTing a fresh running row (which would regress status
// to "in_progress" and blank the conclusion, the incident's exact failure
// shape).
func TestRecordWorkflowJob_LateInProgressPastRetentionDoesNotRegress(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	startedAt := ts(t, 15*24*time.Hour+10*time.Minute)
	completedAt := ts(t, 15*24*time.Hour) // event time past the retention horizon

	require.NoError(t, s.RecordWorkflowJob(ctx, WorkflowJob{
		Owner: "o", Repo: "r", JobID: 9, Name: "lint", Status: "completed",
		Conclusion: "success", StartedAt: startedAt, CompletedAt: completedAt,
	}))
	jobs, err := s.RecentWorkflowJobs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, jobs, 1, "the just-recorded completed row must survive its own call's prune")

	require.NoError(t, s.RecordWorkflowJob(ctx, WorkflowJob{
		Owner: "o", Repo: "r", JobID: 9, Name: "lint", Status: "in_progress",
		StartedAt: startedAt,
	}))
	jobs, err = s.RecentWorkflowJobs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "completed", jobs[0].Status, "late in_progress must not regress a completed job")
	assert.Equal(t, "success", jobs[0].Conclusion)
	assert.Equal(t, completedAt, jobs[0].CompletedAt)
}

// TestRecordWorkflowJob_CompletedWithoutTimestamp verifies the defensive
// completed_at fill: a completed row never stores an empty completed_at (which
// would compare as infinitely old and be pruned on the next write).
func TestRecordWorkflowJob_CompletedWithoutTimestamp(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	require.NoError(t, s.RecordWorkflowJob(ctx, WorkflowJob{
		Owner: "o", Repo: "r", JobID: 1, Name: "no-ts", Status: "completed", Conclusion: "success",
	}))
	// Another write runs the prune; the row must survive it.
	require.NoError(t, s.RecordWorkflowJob(ctx, WorkflowJob{
		Owner: "o", Repo: "r", JobID: 2, Name: "other", Status: "in_progress", StartedAt: ts(t, time.Minute),
	}))

	jobs, err := s.RecentWorkflowJobs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	for _, j := range jobs {
		if j.JobID == 1 {
			assert.NotEmpty(t, j.CompletedAt)
		}
	}
}

// TestRecentWorkflowJobs_Ordering verifies the read order: running jobs first
// (newest started first), then completed jobs (newest completed first).
func TestRecentWorkflowJobs_Ordering(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	require.NoError(t, s.RecordWorkflowJob(ctx, WorkflowJob{
		Owner: "o", Repo: "r", JobID: 1, Name: "done-old", Status: "completed",
		Conclusion: "success", StartedAt: ts(t, 3*time.Hour), CompletedAt: ts(t, 2*time.Hour),
	}))
	require.NoError(t, s.RecordWorkflowJob(ctx, WorkflowJob{
		Owner: "o", Repo: "r", JobID: 2, Name: "done-new", Status: "completed",
		Conclusion: "failure", StartedAt: ts(t, time.Hour), CompletedAt: ts(t, 30*time.Minute),
	}))
	require.NoError(t, s.RecordWorkflowJob(ctx, WorkflowJob{
		Owner: "o", Repo: "r", JobID: 3, Name: "run-old", Status: "in_progress",
		StartedAt: ts(t, 20*time.Minute),
	}))
	require.NoError(t, s.RecordWorkflowJob(ctx, WorkflowJob{
		Owner: "o", Repo: "r", JobID: 4, Name: "run-new", Status: "in_progress",
		StartedAt: ts(t, time.Minute),
	}))

	jobs, err := s.RecentWorkflowJobs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, jobs, 4)
	got := []string{jobs[0].Name, jobs[1].Name, jobs[2].Name, jobs[3].Name}
	assert.Equal(t, []string{"run-new", "run-old", "done-new", "done-old"}, got)

	// The limit caps the row count, preserving the same order.
	jobs, err = s.RecentWorkflowJobs(ctx, 2)
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	assert.Equal(t, "run-new", jobs[0].Name)
	assert.Equal(t, "run-old", jobs[1].Name)
}
