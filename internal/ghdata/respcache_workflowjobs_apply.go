package ghdata

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// A `workflow_job` delivery carries the whole job object, so a moving job
// rewrites entry rather than dropping the page. See docs/cache/rest-routes.md.

// ApplyWorkflowJob writes a `workflow_job` delivery's own job object into every
// cached answer that already lists it: the run's jobs pages, and the
// single-job row. Reports false when NOTHING could be rewritten and the caller
// should flush the run instead.
//
// A repo with no cached rows for the run also reports false; there is no stale
// answer to correct, and the caller's flush is then a no-op.
func (s *Store) ApplyWorkflowJob(ctx context.Context, owner, repo string, runID int64, job StoredWorkflowJob, now time.Time) (bool, error) {
	if runID <= 0 || job.ID <= 0 || job.Status == "" {
		return false, nil
	}
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	rows, err := s.q.ListWorkflowJobsCacheForRun(ctx, dbgen.ListWorkflowJobsCacheForRunParams{
		Owner: ownerKey, Repo: repoKey, RunID: runID,
	})
	if err != nil {
		return false, err
	}
	applied := false
	for _, row := range rows {
		// Another job's single-job row is not about this delivery; skipping it must not drag the run into a flush.
		if row.Kind == WorkflowJobsKindJob && row.RefID != job.ID {
			continue
		}
		patched, liveness, ok := patchWorkflowJobsDoc(row.Kind, row.Doc, job)
		if !ok {
			// A page that does not list the job means the run gained; only a fetch can settle membership.
			if derr := s.q.DeleteWorkflowJobsCacheForRun(ctx, dbgen.DeleteWorkflowJobsCacheForRunParams{
				Owner: ownerKey, Repo: repoKey, RunID: runID,
			}); derr != nil {
				return false, derr
			}
			return false, nil
		}
		if perr := s.PutCachedWorkflowJobs(ctx, ownerKey, repoKey, row.Kind, row.RefID, runID,
			row.PerPage, row.Page, patched, now, liveness.TTL()); perr != nil {
			return false, perr
		}
		applied = true
	}
	return applied, nil
}

// patchWorkflowJobsDoc rewrites stored answer, reporting what is still
// moving in the result (which decides the row's TTL) and whether the rewrite
// was possible at all.
func patchWorkflowJobsDoc(kind, doc string, job StoredWorkflowJob) (string, JobsLiveness, bool) {
	switch kind {
	case WorkflowJobsKindJob:
		var stored StoredWorkflowJob
		if err := json.Unmarshal([]byte(doc), &stored); err != nil || stored.ID != job.ID {
			return "", JobsSettled, false
		}
		if stored.RunAttempt != job.RunAttempt {
			return "", JobsSettled, false // a different attempt is a different set of jobs
		}
		rendered, err := MarshalCacheDoc(job)
		if err != nil {
			return "", JobsSettled, false
		}
		return string(rendered), LivenessOf(job), true

	case WorkflowJobsKindRunJobs:
		var page StoredRunJobsPage
		if err := json.Unmarshal([]byte(doc), &page); err != nil {
			return "", JobsSettled, false
		}
		found := false
		for i := range page.Jobs {
			if page.Jobs[i].ID != job.ID {
				continue
			}
			if page.Jobs[i].RunAttempt != job.RunAttempt {
				// A re-run's membership is settled by a fetch, never by editing an entry from another attempt.
				return "", JobsSettled, false
			}
			// Replaced, not merged: the delivery states this job whole, as view of moment.
			page.Jobs[i] = job
			found = true
		}
		if !found {
			return "", JobsSettled, false
		}
		rendered, err := MarshalCacheDoc(page)
		if err != nil {
			return "", JobsSettled, false
		}
		return string(rendered), LivenessOf(page.Jobs...), true
	}
	return "", JobsSettled, false
}
