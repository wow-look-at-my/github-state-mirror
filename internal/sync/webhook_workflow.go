package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// GitHub Actions deliveries applied to global truth: per-JOB state into the
// workflow_jobs table, and per-RUN state into workflow_runs -- the rows the
// repo-wide runs listing is rebuilt from. Both MAINTAIN their rows
// resource at a time; neither clears a cache (see webhook_invalidate.go for
// why the listing has no flush at all).

// onWorkflowJob records GitHub Actions job state in the global workflow_jobs
// table as it happens. Only in_progress and completed are tracked; the queued
// and waiting actions are deliberately dropped (high-volume churn with no state
// worth keeping). Nothing is invalidated on a bad payload -- no cached resource
// depends on job state.
func (d *WebhookDispatcher) onWorkflowJob(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParseWorkflowJobPayload(event.Raw)
	if err != nil {
		slog.Warn("webhook: failed to parse workflow_job payload", "error", err)
		return ignored("unparseable workflow_job payload")
	}

	// The job names its RUN, and the repo-wide runs listing is rebuilt from
	// run rows. This runs for EVERY action, BEFORE the job table's own
	// queued/waiting filter below: a queued job is exactly a run entering the
	// backlog, which is the answer that listing exists to give. A job payload
	// carries no run-level status, so this may only establish the run's
	// identity and raise its status floor -- never settle it. Best-effort;
	// the job record below is this handler's disposition.
	if err := d.store.ApplyWorkflowRunFromJob(ctx, ghdata.WorkflowJob{
		Owner: payload.Owner, Repo: payload.Repo, RunID: payload.RunID,
		RunAttempt: payload.RunAttempt, WorkflowName: payload.WorkflowName,
		Status: payload.Status, HeadSHA: payload.HeadSHA, HeadBranch: payload.HeadBranch,
	}, time.Now()); err != nil {
		slog.Warn("webhook: apply run state from job failed", "repo", payload.Owner+"/"+payload.Repo, "run", payload.RunID, "error", err)
	}

	if event.Action != "in_progress" && event.Action != "completed" {
		return ignored("workflow_job action " + event.Action + " not tracked")
	}
	if err := d.store.RecordWorkflowJob(ctx, ghdata.WorkflowJob{
		Owner:        payload.Owner,
		Repo:         payload.Repo,
		JobID:        payload.JobID,
		RunID:        payload.RunID,
		RunAttempt:   payload.RunAttempt,
		Name:         payload.Name,
		WorkflowName: payload.WorkflowName,
		Status:       payload.Status,
		Conclusion:   payload.Conclusion,
		HeadSHA:      payload.HeadSHA,
		HeadBranch:   payload.HeadBranch,
		HTMLURL:      payload.HTMLURL,
		StartedAt:    payload.StartedAt,
		CompletedAt:  payload.CompletedAt,
		RunnerName:   payload.RunnerName,
	}); err != nil {
		slog.Warn("webhook: record workflow job failed", "repo", payload.Owner+"/"+payload.Repo, "job", payload.JobID, "error", err)
		return errored("record workflow job failed")
	}
	detail := fmt.Sprintf("job %q %s", payload.Name, payload.Status)
	if payload.Conclusion != "" {
		detail += " (" + payload.Conclusion + ")"
	}
	return applied(detail)
}

// onWorkflowRun applies an Actions RUN's own state to global truth. This is
// the authoritative per-run signal -- the whole object, including the
// run-level status and conclusion no job payload carries -- and the only
// for a run that creates no jobs at all (a startup_failure, or held by a
// concurrency group). The repo-wide runs listing is rebuilt from these rows,
// so this delivery UPDATES the run it names and leaves every other run
// served.
//
// Every action applies, including `requested`: that is a run entering the
// queue, which is exactly what the backlog listing must show.
func (d *WebhookDispatcher) onWorkflowRun(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParseWorkflowRunPayload(event.Raw)
	if err != nil {
		slog.Warn("webhook: failed to parse workflow_run payload", "error", err)
		return ignored("unparseable workflow_run payload")
	}
	if err := d.store.ApplyWorkflowRun(ctx, ghdata.WorkflowRun{
		Owner: payload.Owner, Repo: payload.Repo, RunID: payload.RunID,
		RunAttempt: payload.RunAttempt, Name: payload.Name,
		HeadSHA: payload.HeadSHA, HeadBranch: payload.HeadBranch,
		Status: payload.Status, Conclusion: payload.Conclusion, HTMLURL: payload.HTMLURL,
		CreatedAt: payload.CreatedAt, UpdatedAt: payload.UpdatedAt, RunStartedAt: payload.RunStartedAt,
	}, time.Now()); err != nil {
		slog.Warn("webhook: record workflow run failed", "repo", payload.Owner+"/"+payload.Repo, "run", payload.RunID, "error", err)
		return errored("record workflow run failed")
	}
	detail := fmt.Sprintf("run %d %s", payload.RunID, payload.Status)
	if payload.Conclusion != "" {
		detail += " (" + payload.Conclusion + ")"
	}
	return applied(detail)
}
