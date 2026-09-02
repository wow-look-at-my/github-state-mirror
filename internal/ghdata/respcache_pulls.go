package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/go-containers/set"
)

// Storage layer for the cached PR REST routes; absorbs into and rebuilds

// Single-PR staleness backstop; a stale row misses rather than serves stale.
var PRRowTTL = 24 * time.Hour

// How long a push-invalidated test-merge sha keeps rejecting re-offered answers.
const MergeStaleTTL = time.Hour

// How long the stale marker may reject a CONFLICTING same-sha answer.
const MergeStaleConflictingWindow = 30 * time.Second

// mergeStaleMarkerLive reports whether the row carries a live
// push-invalidated-sha marker (both columns set, stamp inside MergeStaleTTL).
// An unparseable stamp reads as expired: fail open to the plain absorb.
func mergeStaleMarkerLive(pr dbgen.PullRequest, now time.Time) bool {
	if !pr.MergeStaleSha.Valid || pr.MergeStaleSha.String == "" || !pr.MergeStaleAt.Valid {
		return false
	}
	t, err := time.Parse(time.RFC3339, pr.MergeStaleAt.String)
	if err != nil {
		return false
	}
	return now.Sub(t) < MergeStaleTTL
}

// staleShaOffered reports whether offered is exactly the test-merge sha a
// recent push invalidated on the existing row -- presumed pre-push, because
func staleShaOffered(existing dbgen.PullRequest, offered sql.NullString, now time.Time) bool {
	return mergeStaleMarkerLive(existing, now) &&
		offered.Valid && offered.String != "" && offered.String == existing.MergeStaleSha.String
}

// Belt-and-braces: a row holding the push-invalidated sha must miss.
// see docs/cache/merge-stale-sha.md
func PRMergeShaStale(pr dbgen.PullRequest, now time.Time) bool {
	return staleShaOffered(pr, pr.MergeCommitSha, now)
}

// Reports whether the incoming doc's tip proves it post-dates the marking push.
// see docs/cache/merge-stale-sha.md
func pushProvenPostPush(existing, incoming dbgen.PullRequest) bool {
	if !existing.MergeStaleRef.Valid || existing.MergeStaleRef.String == "" ||
		!existing.MergeStaleAfter.Valid || existing.MergeStaleAfter.String == "" {
		return false
	}
	ref, after := existing.MergeStaleRef.String, existing.MergeStaleAfter.String
	if incoming.BaseRefName.Valid && incoming.BaseRefName.String == ref &&
		incoming.BaseRefOid.Valid && incoming.BaseRefOid.String == after {
		return true
	}
	return incoming.HeadRefName.Valid && incoming.HeadRefName.String == ref &&
		incoming.HeadRefOid.Valid && incoming.HeadRefOid.String == after
}

// A CONFLICTING same-sha answer past replica lag is dirty-retained, not stale.
// see docs/cache/merge-stale-sha.md
func conflictingPastReplicaLag(existing, incoming dbgen.PullRequest, now time.Time) bool {
	if !incoming.Mergeable.Valid || incoming.Mergeable.String != "CONFLICTING" {
		return false
	}
	if !existing.MergeStaleAt.Valid {
		return false
	}
	t, err := time.Parse(time.RFC3339, existing.MergeStaleAt.String)
	if err != nil {
		return false
	}
	return now.Sub(t) >= MergeStaleConflictingWindow
}

// GraphQL-sourced rows lack these REST-only fields and must miss.
// see docs/cache/rest-routes.md
func PRRestComplete(pr dbgen.PullRequest) bool {
	return pr.NodeID.Valid && pr.NodeID.String != "" &&
		pr.BaseRefOid.Valid && pr.BaseRefOid.String != "" &&
		pr.AuthorLogin.Valid && pr.AuthorLogin.String != ""
}

// Stale/unparseable touched_at fails to a re-fetch, never to stale state.
func PRRowFresh(pr dbgen.PullRequest, now time.Time) bool {
	if pr.TouchedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, pr.TouchedAt)
	if err != nil {
		return false
	}
	return now.Sub(t) < PRRowTTL
}

// ---- Open-PR list (GET /repos/{owner}/{repo}/pulls) ----

// PullsListFresh reports whether the repo's "open-PR list complete" marker is
// valid. A valid marker means the global pull_requests rows ARE the repo's
// complete open-PR set (webhooks maintain them; the TTL bounds missed
// deliveries). A hit refreshes the marker's LRU timestamp only -- never its
// expiry.
func (s *Store) PullsListFresh(ctx context.Context, owner, repo string, now time.Time) (bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetPullsListMarker(ctx, dbgen.GetPullsListMarkerParams{
		Owner: ownerKey, Repo: repoKey,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return false, nil
	}
	_ = s.q.TouchPullsListMarker(ctx, dbgen.TouchPullsListMarkerParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey,
	})
	return true, nil
}

// HasLivePullsListMarker reports whether the repo's "open-PR list complete"
// marker is currently valid, WITHOUT touching its LRU timestamp -- a pure
// read for the consistency check (which must not mutate the cache), unlike
// PullsListFresh whose hit refreshes last_used_at.
func (s *Store) HasLivePullsListMarker(ctx context.Context, owner, repo string, now time.Time) (bool, error) {
	row, err := s.q.GetPullsListMarker(ctx, dbgen.GetPullsListMarkerParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
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

// RestPullsList returns the repo's open-PR rows (newest-created,
// GitHub's default list order) plus all their labels grouped by PR number.
// owner/repo are matched case-insensitively: rows carry GitHub's canonical
// casing, the request URL may not.
func (s *Store) RestPullsList(ctx context.Context, owner, repo string) ([]dbgen.PullRequest, map[int64][]dbgen.PrLabel, error) {
	prs, err := s.q.ListOpenPullRequestsByRepoNoCase(ctx, dbgen.ListOpenPullRequestsByRepoNoCaseParams{
		Owner: owner, Repo: repo,
	})
	if err != nil {
		return nil, nil, err
	}
	labels, err := s.q.ListPRLabelsByRepoNoCase(ctx, dbgen.ListPRLabelsByRepoNoCaseParams{
		Owner: owner, Repo: repo,
	})
	if err != nil {
		return nil, nil, err
	}
	byPR := make(map[int64][]dbgen.PrLabel, len(prs))
	for _, l := range labels {
		byPR[l.PrNumber] = append(byPR[l.PrNumber], l)
	}
	return prs, byPR, nil
}

// RestSinglePull returns the row for OPEN PR plus its labels, or ok=false
// when no open row exists.
func (s *Store) RestSinglePull(ctx context.Context, owner, repo string, number int64) (dbgen.PullRequest, []dbgen.PrLabel, bool, error) {
	pr, err := s.q.GetOpenPullRequestNoCase(ctx, dbgen.GetOpenPullRequestNoCaseParams{
		Owner: owner, Repo: repo, Number: number,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return dbgen.PullRequest{}, nil, false, nil
	}
	if err != nil {
		return dbgen.PullRequest{}, nil, false, err
	}
	labels, err := s.q.ListPRLabelsNoCase(ctx, dbgen.ListPRLabelsNoCaseParams{
		Owner: owner, Repo: repo, PrNumber: number,
	})
	if err != nil {
		return dbgen.PullRequest{}, nil, false, err
	}
	return pr, labels, true, nil
}

// AbsorbPullsList upserts the PRs of a fetched /pulls list response (and
// their labels) into global truth. When complete is true -- an unfiltered
// page- response that provably holds the WHOLE open set -- it also deletes
// open rows the response no longer contains (PRs closed while unwatched) and
// records the "list complete" marker with the given TTL. The delete honors
// the reconcile grace window: a row touched after fetchStart minus the grace
// was written by a webhook racing the fetch's eventual consistency and
// survives. A filtered or possibly-truncated response absorbs rows only:
// still useful state, but no completeness claim. Rows are written with the
// response's own (canonical) owner/repo casing so they collide with
// webhook/GraphQL-written rows.
func (s *Store) AbsorbPullsList(ctx context.Context, owner, repo string, prs []dbgen.PullRequest, labelsByPR map[int64][]dbgen.PrLabel, complete bool, fetchStart, now time.Time, ttl time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	touched := rfc3339(now)
	fetched := set.New[int64](len(prs))
	for _, pr := range prs {
		fetched.Add(pr.Number)
		applied, err := upsertPRTx(ctx, q, pr, touched)
		if err != nil {
			return err
		}
		if !applied {
			continue
		}
		if err := replacePRLabelsTx(ctx, q, pr.Owner, pr.Repo, pr.Number, labelsByPR[pr.Number]); err != nil {
			return err
		}
	}
	if complete {
		// Drops open rows the complete response omits (closed/gone); grace-windowed.
		cutoff := rfc3339(fetchStart.Add(-reconcileGrace))
		existing, err := q.ListOpenPullRequestsByRepoNoCase(ctx, dbgen.ListOpenPullRequestsByRepoNoCaseParams{
			Owner: owner, Repo: repo,
		})
		if err != nil {
			return err
		}
		for _, row := range existing {
			if fetched.Contains(row.Number) || row.TouchedAt >= cutoff {
				continue
			}
			if err := q.DeletePullRequest(ctx, dbgen.DeletePullRequestParams{
				Owner: row.Owner, Repo: row.Repo, Number: row.Number,
			}); err != nil {
				return err
			}
			if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{
				Owner: row.Owner, Repo: row.Repo, PrNumber: row.Number,
			}); err != nil {
				return err
			}
			// No closure recorded: inferred from absence, not a statement that it closed.
		}
		if err := q.UpsertPullsListMarker(ctx, dbgen.UpsertPullsListMarkerParams{
			Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
			FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredPullsListMarkers(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PrunePullsListMarkersLRU(ctx, CacheMaxRows)
}

// AbsorbSinglePull upserts fetched OPEN PR into global truth. Unlike the
// COALESCE-ing webhook upsert, the fetched mergeable is authoritative --
// including null ("GitHub is recomputing") -- so it is force-set after the
// upsert: a null answer must keep the single-PR route missing (and
// re-fetching) until GitHub resolves it, never resurrect a stale value.
//
//	answer is NOT authoritative: a response whose merge_commit_sha is the
//
// exact sha a branch push just invalidated (a live merge_stale_sha marker).
// The push moved the PR's base or head, and a tip change always changes the
// sha of a SUCCESSFUL test merge -- so GitHub re-offering the invalidated sha
// means its recompute hasn't landed and the WHOLE answer (resolved mergeable
// included) predates the push. Such an answer is stored UNRESOLVED (mergeable
// NULL, merge_commit_sha NULL, marker kept), so reads keep missing -- each
//
//	re-triggering the recompute -- until GitHub serves a NEW sha, which
//
// clears the marker. exemptions overrule that presumption and accept the
// answer, sha and all, marker cleared: () the push-tip proof
// (pushProvenPostPush) -- the answer's reported tip for the marked branch
// equals the marking push's after sha, so it demonstrably post-dates the push
// -- which heals a WRONG mark (the race where the fresh post-push answer was
// absorbed before the late push delivery, which then stamped it stale) on the
// very next poll; and () the dirty-retained pattern
func (s *Store) AbsorbSinglePull(ctx context.Context, pr dbgen.PullRequest, labels []dbgen.PrLabel, now time.Time) (staleRejected bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	existing, err := q.GetPullRequest(ctx, dbgen.GetPullRequestParams{
		Owner: pr.Owner, Repo: pr.Repo, Number: pr.Number,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	staleRejected = err == nil && staleShaOffered(existing, pr.MergeCommitSha, now) &&
		!pushProvenPostPush(existing, pr) && !conflictingPastReplicaLag(existing, pr, now)

	applied, err := upsertPRTx(ctx, q, pr, rfc3339(now))
	if err != nil {
		return false, err
	}
	if !applied {
		return false, tx.Commit()
	}
	mergeable, mergeableState := pr.Mergeable, pr.MergeableState
	if staleRejected {
		// Both facets together: a row left holding a resolved mergeable_state
		mergeable = sql.NullString{}
		mergeableState = sql.NullString{}
	}
	if err := q.SetPRMergeable(ctx, dbgen.SetPRMergeableParams{
		Mergeable:      mergeable,
		MergeableState: mergeableState,
		Owner:          pr.Owner, Repo: pr.Repo, Number: pr.Number,
	}); err != nil {
		return false, err
	}
	if err := replacePRLabelsTx(ctx, q, pr.Owner, pr.Repo, pr.Number, labels); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if staleRejected {
		slog.Info("single-PR absorb: refetch re-offered the push-invalidated test-merge sha; stored unresolved",
			"owner", pr.Owner, "repo", pr.Repo, "number", pr.Number, "sha", pr.MergeCommitSha.String)
	}
	return staleRejected, nil
}

// InvalidatePullsListMarkers drops the repo's "list complete" marker -- the
// structural-event flush (repository renamed/deleted/etc.), NOT something
// pull_request events do (those maintain rows and leave the marker).
func (s *Store) InvalidatePullsListMarkers(ctx context.Context, owner, repo string) error {
	return s.q.DeletePullsListMarkersByRepo(ctx, dbgen.DeletePullsListMarkersByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
