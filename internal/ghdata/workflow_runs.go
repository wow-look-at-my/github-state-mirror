package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// GitHub Actions RUN state -- global truth, maintained by webhooks, and the
// state the repo-wide runs listing is rebuilt from.
//
// The point of this table is that a delivery moving ONE run updates ONE row
// and leaves every other run served. The listing is a filtered view over
// these rows, so a run entering or leaving `queued` changes the answer with
// nothing cleared and nothing re-fetched -- the difference between
// maintaining a cache and invalidating one.
//
// Two writers with deliberately different authority:
//
//   - ApplyWorkflowRun -- a workflow_run delivery or a REST absorb. Both
//     carry the run's own status and conclusion, so both may settle a run.
//   - ApplyWorkflowRunFromJob -- a workflow_job delivery. It names the run
//     but carries no run-level status, so it may only establish identity and
//     RAISE the status floor (an in_progress job proves its run is running).
//     It can never conclude a run: the job set may be incomplete.
//
// WHO may read a rebuilt listing is the reveal layer's job (internal/api).

// workflowRunRetention bounds the table. A completed run the mirror has not
// touched in this long is dropped on the next write; queued and in-progress
// runs are never pruned. Two weeks matches the workflow_jobs window -- long
// enough that a settled run is still answerable, short enough that an
// org's run history cannot grow without limit.
const workflowRunRetention = 14 * 24 * time.Hour

// WorkflowRunsListTTL bounds a listing completeness proof. It is SHORT on
// purpose: the marker's one hole is a run that enters a filter's set with no
// delivery naming it (a run with no jobs yet emits no workflow_job, so only
// a workflow_run delivery names it), and a queued-backlog answer is what a
// runner coordinator provisions against. Every other transition is applied
// to the rows as it happens, so this is a backstop, not the mechanism.
const WorkflowRunsListTTL = 2 * time.Minute

// WorkflowRun is one Actions run's recorded state. Empty string means the
// source did not report the field; the API layer renders those as JSON null
// (name, conclusion until completed, run_started_at until it starts).
type WorkflowRun struct {
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

// WorkflowRunFilter selects a subset of a repo's runs. An empty field means
// "no filter on this field"; the zero value is the unfiltered listing.
type WorkflowRunFilter struct {
	Status     string
	HeadBranch string
	HeadSHA    string
}

// ApplyWorkflowRun records a run from an authoritative source -- a
// workflow_run delivery or a REST absorb. Out-of-order tolerant on the run's
// own updated_at, so a replayed delivery or a listing absorbed while a
// fresher delivery was in flight cannot roll the run's state backwards.
func (s *Store) ApplyWorkflowRun(ctx context.Context, r WorkflowRun, now time.Time) error {
	if err := s.q.UpsertWorkflowRunFull(ctx, dbgen.UpsertWorkflowRunFullParams{
		Owner: NormalizeRepoKey(r.Owner), Repo: NormalizeRepoKey(r.Repo),
		RunID: r.RunID, RunAttempt: r.RunAttempt,
		Name:    r.Name,
		HeadSha: strings.ToLower(r.HeadSHA), HeadBranch: r.HeadBranch,
		Status: r.Status, Conclusion: r.Conclusion, HtmlUrl: r.HTMLURL,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, RunStartedAt: r.RunStartedAt,
		TouchedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	return s.q.PruneSettledWorkflowRuns(ctx, rfc3339(now.Add(-workflowRunRetention)))
}

// ApplyWorkflowRunFromJob records what a workflow_job delivery proves about
// its RUN: the run exists, its identity, and -- when the job is running --
// that the run is running too. It never settles a run and never regresses
// one an authoritative writer already settled.
func (s *Store) ApplyWorkflowRunFromJob(ctx context.Context, j WorkflowJob, now time.Time) error {
	if j.RunID <= 0 {
		return nil
	}
	// A job's own status maps onto the strongest run-level claim it can
	// support: a running job means a running run; anything else (queued,
	// completed) leaves the run at the floor, because one job's state says
	// nothing about the others'.
	status := "queued"
	if j.Status == "in_progress" {
		status = "in_progress"
	}
	return s.q.UpsertWorkflowRunFromJob(ctx, dbgen.UpsertWorkflowRunFromJobParams{
		Owner: NormalizeRepoKey(j.Owner), Repo: NormalizeRepoKey(j.Repo),
		RunID: j.RunID, RunAttempt: j.RunAttempt,
		Name:    j.WorkflowName,
		HeadSha: strings.ToLower(j.HeadSHA), HeadBranch: j.HeadBranch,
		Status:    status,
		TouchedAt: rfc3339(now),
	})
}

// ListWorkflowRuns returns one page of a repo's runs matching the filter,
// newest first (GitHub's own ordering), together with the total number of
// matches. The count is exact only because the caller checked the
// completeness marker first -- rows alone never prove a list.
func (s *Store) ListWorkflowRuns(ctx context.Context, owner, repo string, f WorkflowRunFilter, perPage, page int) ([]WorkflowRun, int64, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	shaKey := strings.ToLower(f.HeadSHA)
	total, err := s.q.CountWorkflowRuns(ctx, dbgen.CountWorkflowRunsParams{
		Owner: ownerKey, Repo: repoKey,
		Status: f.Status, HeadBranch: f.HeadBranch, HeadSha: shaKey,
	})
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.q.ListWorkflowRuns(ctx, dbgen.ListWorkflowRunsParams{
		Owner: ownerKey, Repo: repoKey,
		Status: f.Status, HeadBranch: f.HeadBranch, HeadSha: shaKey,
		PageSize: int64(perPage), PageOffset: int64((page - 1) * perPage),
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]WorkflowRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, WorkflowRun{
			Owner: row.Owner, Repo: row.Repo, RunID: row.RunID, RunAttempt: row.RunAttempt,
			Name: row.Name, HeadSHA: row.HeadSha, HeadBranch: row.HeadBranch,
			Status: row.Status, Conclusion: row.Conclusion, HTMLURL: row.HtmlUrl,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, RunStartedAt: row.RunStartedAt,
		})
	}
	return out, total, nil
}

// ReconcileWorkflowRuns drops rows a complete response contradicts: a run
// that still MATCHES the filter in truth but was absent from a short page-1
// answer for that same filter has provably moved, and the answer did not say
// where to. Deleting it is the honest correction -- the next read re-absorbs
// the run under whatever state it now has.
func (s *Store) ReconcileWorkflowRuns(ctx context.Context, owner, repo string, f WorkflowRunFilter, kept []int64) error {
	// NOT IN () is a syntax error in SQLite and an empty response means every
	// matching row is stale, so pass a sentinel that can never be a run id.
	if len(kept) == 0 {
		kept = []int64{-1}
	}
	return s.q.DeleteWorkflowRunsNotIn(ctx, dbgen.DeleteWorkflowRunsNotInParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
		Status: f.Status, HeadBranch: f.HeadBranch, HeadSha: strings.ToLower(f.HeadSHA),
		Kept: kept,
	})
}

// WorkflowRunsListComplete reports whether truth provably holds every run
// matching this filter -- i.e. a short page-1 answer for exactly this filter
// was absorbed recently enough that the marker has not expired.
func (s *Store) WorkflowRunsListComplete(ctx context.Context, owner, repo, filters string, now time.Time) (bool, error) {
	row, err := s.q.GetWorkflowRunsListMarker(ctx, dbgen.GetWorkflowRunsListMarkerParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Filters: filters,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	exp, perr := time.Parse(time.RFC3339, row.ExpiresAt)
	return perr == nil && exp.After(now), nil
}

// MarkWorkflowRunsListComplete records the completeness proof for one filter
// set. Only a short page-1 absorb may call this; a run DELIVERY must never
// touch it, because deliveries maintain the rows the marker vouches for.
func (s *Store) MarkWorkflowRunsListComplete(ctx context.Context, owner, repo, filters string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertWorkflowRunsListMarker(ctx, dbgen.UpsertWorkflowRunsListMarkerParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Filters: filters,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)),
	}); err != nil {
		return err
	}
	return s.q.DeleteExpiredWorkflowRunsListMarkers(ctx, rfc3339(now))
}

// InvalidateWorkflowRunsTruth drops a repo's run rows AND its completeness
// proofs. This is the `repository` event's flush (rename, delete, visibility
// change) -- the only event that invalidates rather than maintains.
func (s *Store) InvalidateWorkflowRunsTruth(ctx context.Context, owner, repo string) error {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	if err := s.q.DeleteWorkflowRunsByRepo(ctx, dbgen.DeleteWorkflowRunsByRepoParams{
		Owner: ownerKey, Repo: repoKey,
	}); err != nil {
		return err
	}
	return s.q.DeleteWorkflowRunsListMarkers(ctx, dbgen.DeleteWorkflowRunsListMarkersParams{
		Owner: ownerKey, Repo: repoKey,
	})
}
