package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/go-containers/set"
)

// GitHub Actions run state: global truth, maintained by webhooks.

// workflowRunRetention: a completed run untouched this long is dropped;
const workflowRunRetention = 14 * 24 * time.Hour

// WorkflowRunsListTTL is a backstop, not the mechanism: see docs/cache/rest-routes.md
const WorkflowRunsListTTL = 2 * time.Minute

// WorkflowRun is Actions run's recorded state. Empty string means the
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

// WorkflowRunFilter: an empty field means no filter; the value is unfiltered.
type WorkflowRunFilter struct {
	Status     string
	HeadBranch string
	HeadSHA    string
}

// ApplyWorkflowRun records a run from an authoritative source -- a
// workflow_run delivery or a REST absorb. Out-of-order tolerant on the run's
// own updated_at. This is so a replayed delivery or a listing absorbed while
// a fresher delivery was in flight cannot roll the run's state backwards.
func (s *Store) ApplyWorkflowRun(ctx context.Context, r WorkflowRun, now time.Time) error {
	if err := s.qx(ctx).UpsertWorkflowRunFull(ctx, dbgen.UpsertWorkflowRunFullParams{
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
	return s.qx(ctx).PruneSettledWorkflowRuns(ctx, rfc3339(now.Add(-workflowRunRetention)))
}

// ApplyWorkflowRunFromJob never settles or regresses a run; it only raises
// the status floor a workflow_job delivery can prove.
func (s *Store) ApplyWorkflowRunFromJob(ctx context.Context, j WorkflowJob, now time.Time) error {
	if j.RunID <= 0 {
		return nil
	}
	//  job's state says nothing about the others'.
	status := "queued"
	if j.Status == "in_progress" {
		status = "in_progress"
	}
	return s.qx(ctx).UpsertWorkflowRunFromJob(ctx, dbgen.UpsertWorkflowRunFromJobParams{
		Owner: NormalizeRepoKey(j.Owner), Repo: NormalizeRepoKey(j.Repo),
		RunID: j.RunID, RunAttempt: j.RunAttempt,
		Name:    j.WorkflowName,
		HeadSha: strings.ToLower(j.HeadSHA), HeadBranch: j.HeadBranch,
		Status:    status,
		TouchedAt: rfc3339(now),
	})
}

// ListWorkflowRuns returns page of a repo's runs matching the filter,
// newest (GitHub's own ordering), together with the total number of
// matches. The count is exact only because the caller checked the
// completeness marker -- rows alone never prove a list.
func (s *Store) ListWorkflowRuns(ctx context.Context, owner, repo string, f WorkflowRunFilter, perPage, page int) ([]WorkflowRun, int64, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	shaKey := strings.ToLower(f.HeadSHA)
	total, err := s.qx(ctx).CountWorkflowRuns(ctx, dbgen.CountWorkflowRunsParams{
		Owner: ownerKey, Repo: repoKey,
		Status: f.Status, HeadBranch: f.HeadBranch, HeadSha: shaKey,
	})
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.qx(ctx).ListWorkflowRuns(ctx, dbgen.ListWorkflowRunsParams{
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

// WorkflowRunListingLag bounds how far GitHub's runs listing trails the deliveries about the same runs.
const WorkflowRunListingLag = time.Minute

// workflowRunsScanPage is the page size ReconcileWorkflowRuns reads truth in.
const workflowRunsScanPage = 500

// ReconcileWorkflowRuns drops rows a complete response contradicts. This is
// a run that still MATCHES. The filter in truth but was absent from a short
// page- answer for that same filter has provably moved. The answer did not
// say where to. Deleting it is the honest correction -- the next read
// re-absorbs the run under whatever state it now has.
//
// A row touched within WorkflowRunListingLag of fetchedAt (the instant the
// fetch began) is newer than the answer can be trusted to reflect, so it is
// kept. Its id is returned: a non-empty result means the answer is behind
// truth and must not be stored as a complete picture.
func (s *Store) ReconcileWorkflowRuns(ctx context.Context, owner, repo string, f WorkflowRunFilter, kept []int64, fetchedAt time.Time) ([]int64, error) {
	ahead, err := s.workflowRunsTouchedSince(ctx, owner, repo, f, fetchedAt.Add(-WorkflowRunListingLag), kept)
	if err != nil {
		return nil, err
	}
	spared := make([]int64, 0, len(kept)+len(ahead)+1)
	spared = append(spared, kept...)
	spared = append(spared, ahead...)
	// NOT IN () is a syntax error in SQLite. An empty response means every
	// matching row is stale, so pass a sentinel that can never be a run id.
	if len(spared) == 0 {
		spared = append(spared, -1)
	}
	return ahead, s.qx(ctx).DeleteWorkflowRunsNotIn(ctx, dbgen.DeleteWorkflowRunsNotInParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
		Status: f.Status, HeadBranch: f.HeadBranch, HeadSha: strings.ToLower(f.HeadSHA),
		Kept: spared,
	})
}

// workflowRunsTouchedSince returns the ids of rows matching f, absent from
// listed, and touched at or after since.
func (s *Store) workflowRunsTouchedSince(ctx context.Context, owner, repo string, f WorkflowRunFilter, since time.Time, listed []int64) ([]int64, error) {
	inAnswer := set.New[int64](len(listed))
	for _, id := range listed {
		inAnswer.Add(id)
	}
	// touched_at is stored at second precision.
	floor := since.UTC().Truncate(time.Second)
	var out []int64
	for offset := int64(0); ; offset += workflowRunsScanPage {
		rows, err := s.qx(ctx).ListWorkflowRuns(ctx, dbgen.ListWorkflowRunsParams{
			Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
			Status: f.Status, HeadBranch: f.HeadBranch, HeadSha: strings.ToLower(f.HeadSHA),
			PageSize: workflowRunsScanPage, PageOffset: offset,
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if inAnswer.Contains(row.RunID) {
				continue
			}
			touched, err := time.Parse(time.RFC3339, row.TouchedAt)
			if err != nil {
				return nil, fmt.Errorf("workflow run %d: touched_at %q: %w", row.RunID, row.TouchedAt, err)
			}
			if !touched.Before(floor) {
				out = append(out, row.RunID)
			}
		}
		if len(rows) < workflowRunsScanPage {
			return out, nil
		}
	}
}

// WorkflowRunsListComplete reports whether truth provably holds every run
// matching this filter -- i.e. a short page- answer for exactly this filter
// was absorbed recently enough. The marker has not expired.
func (s *Store) WorkflowRunsListComplete(ctx context.Context, owner, repo, filters string, now time.Time) (bool, error) {
	row, err := s.qx(ctx).GetWorkflowRunsListMarker(ctx, dbgen.GetWorkflowRunsListMarkerParams{
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

// MarkWorkflowRunsListComplete records the completeness proof for filter
// set. Only a short page- absorb may call this; a run DELIVERY must never
// touch it, because deliveries maintain the rows the marker vouches for.
func (s *Store) MarkWorkflowRunsListComplete(ctx context.Context, owner, repo, filters string, now time.Time, ttl time.Duration) error {
	if err := s.qx(ctx).UpsertWorkflowRunsListMarker(ctx, dbgen.UpsertWorkflowRunsListMarkerParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Filters: filters,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)),
	}); err != nil {
		return err
	}
	return s.qx(ctx).DeleteExpiredWorkflowRunsListMarkers(ctx, rfc3339(now))
}

// InvalidateWorkflowRunsTruth drops a repo's run rows AND its completeness
// proofs. This is the `repository` event's flush (rename, delete, visibility
// change) -- the only event that invalidates rather than maintains.
func (s *Store) InvalidateWorkflowRunsTruth(ctx context.Context, owner, repo string) error {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	if err := s.qx(ctx).DeleteWorkflowRunsByRepo(ctx, dbgen.DeleteWorkflowRunsByRepoParams{
		Owner: ownerKey, Repo: repoKey,
	}); err != nil {
		return err
	}
	return s.qx(ctx).DeleteWorkflowRunsListMarkers(ctx, dbgen.DeleteWorkflowRunsListMarkersParams{
		Owner: ownerKey, Repo: repoKey,
	})
}
