package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The fixture timestamps are relative to now so they can never age past the
// 14-day completed-job retention (ghdata's workflowJobRetention): hardcoded
// dates rotted on 2026-07-17, when RecordWorkflowJob's on-write prune started
// deleting the just-upserted completed rows mid-test. Truncated to the second
// so the RFC3339 strings round-trip byte-identically through payload → store.
var (
	wfjStartedAt   = time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second).Format(time.RFC3339)
	wfjCompletedAt = time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second).Format(time.RFC3339)
)

// makeWorkflowJobPayload builds a realistic workflow_job webhook body. status
// "in_progress" carries null conclusion/completed_at; "completed" fills both.
func makeWorkflowJobPayload(t *testing.T, action, owner, repo string, jobID int64, name, status, conclusion string) json.RawMessage {
	t.Helper()
	job := map[string]interface{}{
		"id":            jobID,
		"run_id":        int64(555),
		"run_attempt":   int64(1),
		"workflow_name": "CI",
		"head_branch":   "main",
		"head_sha":      "cafe1234",
		"html_url":      "https://github.com/" + owner + "/" + repo + "/actions/runs/555/job/1",
		"status":        status,
		"conclusion":    nil,
		"name":          name,
		"started_at":    wfjStartedAt,
		"completed_at":  nil,
		"runner_name":   "runner-1",
	}
	if conclusion != "" {
		job["conclusion"] = conclusion
	}
	if status == "completed" {
		job["completed_at"] = wfjCompletedAt
	}
	data, err := json.Marshal(map[string]interface{}{
		"action":       action,
		"workflow_job": job,
		"repository":   map[string]interface{}{"name": repo, "owner": map[string]interface{}{"login": owner}},
	})
	require.Nil(t, err)
	return data
}

// TestDispatch_WorkflowJob_InProgressThenCompleted covers the normal event
// order: in_progress inserts the running row, completed upserts the verdict.
// The table is global, so no repo needs to be cached in any scope first.
func TestDispatch_WorkflowJob_InProgressThenCompleted(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	result := dispatcher.Dispatch(ctx, webhook.ParseEvent("workflow_job",
		makeWorkflowJobPayload(t, "in_progress", "my-org", "my-repo", 42, "build", "in_progress", "")))
	assert.Equal(t, webhook.DispApplied, result.Disposition)
	assert.Equal(t, http.StatusOK, result.StatusCode())

	jobs, err := store.RecentWorkflowJobs(ctx, 10)
	require.Nil(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "in_progress", jobs[0].Status)
	assert.Equal(t, "build", jobs[0].Name)
	assert.Equal(t, "CI", jobs[0].WorkflowName)
	assert.Equal(t, wfjStartedAt, jobs[0].StartedAt)
	assert.Equal(t, "", jobs[0].Conclusion)
	assert.Equal(t, "runner-1", jobs[0].RunnerName)

	result = dispatcher.Dispatch(ctx, webhook.ParseEvent("workflow_job",
		makeWorkflowJobPayload(t, "completed", "my-org", "my-repo", 42, "build", "completed", "success")))
	assert.Equal(t, webhook.DispApplied, result.Disposition)

	jobs, err = store.RecentWorkflowJobs(ctx, 10)
	require.Nil(t, err)
	require.Len(t, jobs, 1, "same job id must upsert, not duplicate")
	assert.Equal(t, "completed", jobs[0].Status)
	assert.Equal(t, "success", jobs[0].Conclusion)
	assert.Equal(t, wfjCompletedAt, jobs[0].CompletedAt)

	// Both deliveries land in the global webhook log.
	deliveries, err := store.RecentWebhookDeliveries(ctx, 10)
	require.Nil(t, err)
	require.Len(t, deliveries, 2)
	assert.Equal(t, "workflow_job", deliveries[0].EventType)
	assert.Equal(t, webhook.DispApplied, deliveries[0].Disposition)
}

// TestDispatch_WorkflowJob_CompletedBeforeInProgress covers out-of-order
// delivery: a completed event with no prior row still records the full job.
func TestDispatch_WorkflowJob_CompletedBeforeInProgress(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	result := dispatcher.Dispatch(ctx, webhook.ParseEvent("workflow_job",
		makeWorkflowJobPayload(t, "completed", "my-org", "my-repo", 7, "test", "completed", "failure")))
	assert.Equal(t, webhook.DispApplied, result.Disposition)

	jobs, err := store.RecentWorkflowJobs(ctx, 10)
	require.Nil(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "completed", jobs[0].Status)
	assert.Equal(t, "failure", jobs[0].Conclusion)
	assert.Equal(t, wfjStartedAt, jobs[0].StartedAt)
	assert.Equal(t, wfjCompletedAt, jobs[0].CompletedAt)
}

// TestDispatch_WorkflowJob_LateInProgressDoesNotRegress covers the other
// out-of-order case: an in_progress arriving after completed must not drag the
// row back to running or wipe its conclusion/completed_at.
func TestDispatch_WorkflowJob_LateInProgressDoesNotRegress(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	dispatcher.Dispatch(ctx, webhook.ParseEvent("workflow_job",
		makeWorkflowJobPayload(t, "completed", "my-org", "my-repo", 9, "lint", "completed", "success")))
	dispatcher.Dispatch(ctx, webhook.ParseEvent("workflow_job",
		makeWorkflowJobPayload(t, "in_progress", "my-org", "my-repo", 9, "lint", "in_progress", "")))

	jobs, err := store.RecentWorkflowJobs(ctx, 10)
	require.Nil(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "completed", jobs[0].Status, "late in_progress must not regress a completed job")
	assert.Equal(t, "success", jobs[0].Conclusion)
	assert.Equal(t, wfjCompletedAt, jobs[0].CompletedAt)
}

// TestDispatch_WorkflowJob_QueuedIgnored verifies the untracked actions
// (queued/waiting) are dropped as "ignored" without touching the table.
func TestDispatch_WorkflowJob_QueuedIgnored(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	for _, action := range []string{"queued", "waiting"} {
		result := dispatcher.Dispatch(ctx, webhook.ParseEvent("workflow_job",
			makeWorkflowJobPayload(t, action, "my-org", "my-repo", 11, "build", "queued", "")))
		assert.Equal(t, webhook.DispIgnored, result.Disposition)
		assert.Equal(t, http.StatusAccepted, result.StatusCode())
	}

	jobs, err := store.RecentWorkflowJobs(ctx, 10)
	require.Nil(t, err)
	assert.Empty(t, jobs)
}

// TestDispatch_WorkflowJob_UnparseableIgnored verifies a tracked action with a
// junk payload is "ignored" (nothing to invalidate for job state), not an error.
func TestDispatch_WorkflowJob_UnparseableIgnored(t *testing.T) {
	dispatcher, _, _, _ := setupDispatcher(t)
	ctx := context.Background()

	event := webhook.Event{Type: "workflow_job", Action: "completed", Raw: json.RawMessage(`{}`)}
	result := dispatcher.Dispatch(ctx, event)

	assert.Equal(t, webhook.DispIgnored, result.Disposition)
	assert.Equal(t, http.StatusAccepted, result.StatusCode())
}
