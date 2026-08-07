package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// GitHub Actions deliveries through the dispatcher. The per-COMMIT runs
// snapshot is flushed by head_sha like any other ref-keyed cache; the
// repo-wide runs LISTING is not flushed at all, because its rows are
// MAINTAINED per run -- that distinction is what these tests pin.

// TestDispatch_WorkflowRun_FlushesWorkflowRunsForSHA: a workflow_run delivery
// flushes its head_sha's cached workflow-runs pages -- the ONLY invalidation
// signal for a startup_failure run, which creates no jobs, check runs, or
// statuses. The delivery ALSO applies the run to truth (that is what the
// repo-wide listing reads), so it records as applied.
func TestDispatch_WorkflowRun_FlushesWorkflowRunsForSHA(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	s := r2Seeder{t: t, store: store, now: time.Now()}

	s.seedWorkflowRuns(r2SHA)
	s.seedWorkflowRuns(r2OtherSHA)

	result := dispatcher.Dispatch(ctx, webhook.ParseEvent("workflow_run", []byte(`{
		"action": "completed",
		"workflow_run": {"id": 99, "head_sha": "`+r2SHA+`", "status": "completed",
			"conclusion": "startup_failure"},
		"repository": {"name": "repo1", "owner": {"login": "org1"}}
	}`)))

	assert.Equal(t, webhook.DispApplied, result.Disposition, "the run's own state is applied to truth")
	assert.False(t, s.workflowRunsServe(r2SHA), "the run's head sha's workflow-runs pages must flush")
	assert.True(t, s.workflowRunsServe(r2OtherSHA), "another sha's workflow-runs pages must survive")
}

// TestDispatch_WorkflowRun_AppliesRunState is the property the repo-wide runs
// listing depends on: a workflow_run delivery UPDATES THE ONE RUN it names
// and leaves every other run answerable. Nothing is cleared, so the backlog
// view changes by exactly one row -- the difference between maintaining a
// cache and invalidating one.
func TestDispatch_WorkflowRun_AppliesRunState(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	s := r2Seeder{t: t, store: store, now: time.Now()}
	s.seedRun(1, "queued")
	s.seedRun(2, "queued")

	result := dispatcher.Dispatch(context.Background(), webhook.ParseEvent("workflow_run", []byte(`{
		"action": "in_progress",
		"workflow_run": {"id": 2, "run_attempt": 1, "name": "CI", "head_sha": "`+r2SHA+`",
			"head_branch": "feat", "status": "in_progress", "conclusion": null,
			"html_url": "https://github.com/org1/repo1/actions/runs/2",
			"created_at": "2026-07-01T10:00:00Z", "updated_at": "2026-07-01T10:06:00Z",
			"run_started_at": "2026-07-01T10:06:00Z"},
		"repository": {"name": "repo1", "owner": {"login": "org1"}}
	}`)))

	assert.Equal(t, webhook.DispApplied, result.Disposition,
		"a workflow_run delivery maintains truth; it is not an ignored flush signal")
	assert.Equal(t, map[int64]string{1: "queued", 2: "in_progress"}, s.runStatuses(),
		"only the named run moved -- run 1 is still served from its row")
}

// TestDispatch_WorkflowRunRequested_EntersTheBacklog: `requested` is a run
// ENTERING the queue, and it is the only delivery for a run that creates no
// jobs at all (a startup_failure, or one held by a concurrency group). It has
// to apply, or the backlog never learns about that run.
func TestDispatch_WorkflowRunRequested_EntersTheBacklog(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	s := r2Seeder{t: t, store: store, now: time.Now()}

	dispatcher.Dispatch(context.Background(), webhook.ParseEvent("workflow_run", []byte(`{
		"action": "requested",
		"workflow_run": {"id": 5, "name": "CI", "head_sha": "`+r2SHA+`", "head_branch": "feat",
			"status": "queued", "conclusion": null, "created_at": "2026-07-01T10:00:00Z",
			"updated_at": "2026-07-01T10:00:00Z", "run_started_at": null},
		"repository": {"name": "repo1", "owner": {"login": "org1"}}
	}`)))

	assert.Equal(t, map[int64]string{5: "queued"}, s.runStatuses())
}

// TestDispatch_WorkflowJob_MaintainsItsRun: a job delivery names its RUN, so
// it maintains that run's row -- for EVERY action, including the queued one
// the job table drops as ignored (a queued job is a run entering the
// backlog). It carries no run-level status, so it may only raise the floor.
func TestDispatch_WorkflowJob_MaintainsItsRun(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	s := r2Seeder{t: t, store: store, now: time.Now()}
	s.seedRun(1, "queued")

	// makeWorkflowJobPayload pins run id 555 (42 is the JOB id).
	result := dispatcher.Dispatch(context.Background(), webhook.ParseEvent("workflow_job",
		makeWorkflowJobPayload(t, "queued", "org1", "repo1", 42, "build", "queued", "")))

	assert.Equal(t, webhook.DispIgnored, result.Disposition, "queued stays ignored for the job table")
	assert.Equal(t, map[int64]string{1: "queued", 555: "queued"}, s.runStatuses(),
		"the job's run enters the backlog; the unrelated run is untouched")

	dispatcher.Dispatch(context.Background(), webhook.ParseEvent("workflow_job",
		makeWorkflowJobPayload(t, "in_progress", "org1", "repo1", 42, "build", "in_progress", "")))
	assert.Equal(t, map[int64]string{1: "queued", 555: "in_progress"}, s.runStatuses(),
		"a running job raises its run's floor, still touching nothing else")
}

// TestDispatch_RunStateEvents_NeverClearTheListing pins the doctrine the
// first cut of this route violated: run-state deliveries MAINTAIN the run
// rows, so none of them may clear the repo's rows or its completeness proof.
// Only `repository` invalidates. If this regresses, every job event in a busy
// repo throws away every other run's answer -- invalidate-and-refetch wearing
// a cache's clothes.
func TestDispatch_RunStateEvents_NeverClearTheListing(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		payload     string
	}{{
		name:  "check_run completed",
		event: "check_run",
		payload: `{"action": "completed",
			"check_run": {"head_sha": "` + r2SHA + `", "status": "completed", "conclusion": "success",
				"name": "build", "check_suite": {"head_branch": "feat"}},
			"repository": {"name": "repo1", "owner": {"login": "org1"}}}`,
	}, {
		name:  "check_suite requested",
		event: "check_suite",
		payload: `{"action": "requested",
			"check_suite": {"head_sha": "` + r2SHA + `", "status": "queued", "head_branch": "feat"},
			"repository": {"name": "repo1", "owner": {"login": "org1"}}}`,
	}, {
		name:    "workflow_job queued",
		event:   "workflow_job",
		payload: "",
	}, {
		name:  "workflow_run completed",
		event: "workflow_run",
		payload: `{"action": "completed",
			"workflow_run": {"id": 99, "head_sha": "` + r2SHA + `", "status": "completed",
				"conclusion": "success", "created_at": "2026-07-01T10:00:00Z",
				"updated_at": "2026-07-01T10:05:00Z"},
			"repository": {"name": "repo1", "owner": {"login": "org1"}}}`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dispatcher, _, _, store := setupDispatcher(t)
			ctx := context.Background()
			s := r2Seeder{t: t, store: store, now: time.Now()}
			s.seedRun(1, "queued")
			require.NoError(t, store.MarkWorkflowRunsListComplete(ctx, "org1", "repo1", "status=queued", s.now, time.Minute))

			raw := []byte(tc.payload)
			if tc.event == "workflow_job" {
				raw = makeWorkflowJobPayload(t, "queued", "org1", "repo1", 42, "build", "queued", "")
			}
			dispatcher.Dispatch(ctx, webhook.ParseEvent(tc.event, raw))

			assert.Contains(t, s.runStatuses(), int64(1), "an unrelated run's row must survive")
			complete, err := store.WorkflowRunsListComplete(ctx, "org1", "repo1", "status=queued", s.now)
			require.NoError(t, err)
			assert.True(t, complete, "a run delivery maintains the rows; it must never clear the proof")
		})
	}

	// repository is the one event that DOES invalidate: a rename, delete, or
	// visibility change makes the rows themselves wrong, not merely stale.
	t.Run("repository is the exception", func(t *testing.T) {
		dispatcher, _, _, store := setupDispatcher(t)
		ctx := context.Background()
		s := r2Seeder{t: t, store: store, now: time.Now()}
		s.seedRun(1, "queued")
		require.NoError(t, store.MarkWorkflowRunsListComplete(ctx, "org1", "repo1", "status=queued", s.now, time.Minute))

		dispatcher.Dispatch(ctx, webhook.ParseEvent("repository", []byte(`{
			"action": "privatized",
			"repository": {"name": "repo1", "full_name": "org1/repo1", "private": true,
				"visibility": "private", "owner": {"login": "org1"}}
		}`)))

		assert.Empty(t, s.runStatuses(), "a repository event drops the run rows")
		complete, err := store.WorkflowRunsListComplete(ctx, "org1", "repo1", "status=queued", s.now)
		require.NoError(t, err)
		assert.False(t, complete, "...and the completeness proof with them")
	})
}

// TestFlushWorkflowRunsForSHA_EmptySHA_FallsBackRepoWide: the shared
// workflow-runs flush widens to the whole repo when the caller has no sha --
// an empty sha would exact-match nothing, silently flushing NOTHING while the
// triggering payload still said some run changed. (ParseCheckPayload requires
// a sha today, so the CI-event path cannot reach this; the fallback is what
// keeps a future parser relaxation from turning that flush into a no-op.)
func TestFlushWorkflowRunsForSHA_EmptySHA_FallsBackRepoWide(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	s := r2Seeder{t: t, store: store, now: time.Now()}

	s.seedWorkflowRuns(r2SHA)
	s.seedWorkflowRuns(r2OtherSHA)

	dispatcher.flushWorkflowRunsForSHA(ctx, "org1/repo1", "org1", "repo1", "")

	assert.False(t, s.workflowRunsServe(r2SHA), "an empty sha must flush repo-wide, not no-op")
	assert.False(t, s.workflowRunsServe(r2OtherSHA), "an empty sha must flush repo-wide, not no-op")
}
