package ghdata

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// A `workflow_job` delivery carries the WHOLE job object -- id, status,
// conclusion, labels, runner name, every step, both timestamps -- which is
// exactly what the cached job answers hold. So a job moving is not a reason to
// drop those rows; it is the new value for one entry inside them.
//
// This is what lets the mirror serve a run that is still running. A fetch
// proves a page's MEMBERSHIP (which jobs belong to the run, in which order);
// the deliveries that follow keep each entry's contents current. Neither half
// works alone: webhooks cannot prove a set is complete, and a TTL short enough
// to track a live run would mean fetching as often as not caching at all.
//
// Anything the stored page cannot absorb falls back to the flush the caller
// would have done anyway -- a job the page does not list (the run's membership
// changed, which only a fetch can settle), or a payload whose job the model
// cannot hold.

// ApplyWorkflowJob writes a `workflow_job` delivery's own job object into every
// cached answer that already lists it: the run's jobs pages, and the
// single-job row. Reports false when NOTHING could be rewritten and the caller
// should flush the run instead.
//
// A repo with no cached rows for the run also reports false; there is no stale
// answer to correct, and the caller's flush is then a no-op.
func (s *Store) ApplyWorkflowJob(ctx context.Context, owner, repo string, runID int64, job StoredWorkflowJob, now time.Time, liveTTL, terminalTTL time.Duration) (bool, error) {
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
		// A single-job row is keyed by ITS job id, and a run has many. Another
		// job's row is not about this delivery at all -- skipping it is not a
		// failure to absorb, so it must not drag the run into the flush.
		if row.Kind == WorkflowJobsKindJob && row.RefID != job.ID {
			continue
		}
		patched, live, ok := patchWorkflowJobsDoc(row.Kind, row.Doc, job)
		if !ok {
			// This row cannot represent the delivery -- most often a page that
			// does not list the job, meaning the run gained one and only a
			// fetch can say where it belongs. Drop this row; the others may
			// still be rewritable.
			if derr := s.q.DeleteWorkflowJobsCacheForRun(ctx, dbgen.DeleteWorkflowJobsCacheForRunParams{
				Owner: ownerKey, Repo: repoKey, RunID: runID,
			}); derr != nil {
				return false, derr
			}
			return false, nil
		}
		ttl := terminalTTL
		if live {
			ttl = liveTTL
		}
		if perr := s.PutCachedWorkflowJobs(ctx, ownerKey, repoKey, row.Kind, row.RefID, runID,
			row.PerPage, row.Page, patched, now, ttl); perr != nil {
			return false, perr
		}
		applied = true
	}
	return applied, nil
}

// patchWorkflowJobsDoc rewrites one stored answer, reporting whether anything
// in the result is still moving (which decides the row's TTL) and whether the
// rewrite was possible at all.
func patchWorkflowJobsDoc(kind, doc string, job StoredWorkflowJob) (string, bool, bool) {
	switch kind {
	case WorkflowJobsKindJob:
		var stored StoredWorkflowJob
		if err := json.Unmarshal([]byte(doc), &stored); err != nil || stored.ID != job.ID {
			return "", false, false
		}
		if stored.RunAttempt != job.RunAttempt {
			return "", false, false // a different attempt is a different set of jobs
		}
		rendered, err := MarshalCacheDoc(job)
		if err != nil {
			return "", false, false
		}
		return string(rendered), !JobIsTerminal(job), true

	case WorkflowJobsKindRunJobs:
		var page StoredRunJobsPage
		if err := json.Unmarshal([]byte(doc), &page); err != nil {
			return "", false, false
		}
		found := false
		live := false
		for i := range page.Jobs {
			if page.Jobs[i].ID == job.ID && page.Jobs[i].RunAttempt != job.RunAttempt {
				// A RE-RUN reports a new attempt. Whether it reuses job ids or
				// mints them, the page's membership is settled by a fetch, not
				// by editing one entry into a set from another attempt.
				return "", false, false
			}
			if page.Jobs[i].ID == job.ID {
				// The delivery states this job whole, so the entry is
				// REPLACED rather than merged: a merge would have to decide
				// which of two views of a field is newer, and the payload is
				// one view of one moment.
				page.Jobs[i] = job
				found = true
			}
			if !JobIsTerminal(page.Jobs[i]) {
				live = true
			}
		}
		if !found {
			return "", false, false
		}
		rendered, err := MarshalCacheDoc(page)
		if err != nil {
			return "", false, false
		}
		return string(rendered), live, true
	}
	return "", false, false
}
