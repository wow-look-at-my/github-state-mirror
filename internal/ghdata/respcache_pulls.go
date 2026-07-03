package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the cached PR REST routes:
//
//   GET /repos/{owner}/{repo}/pulls          (the open-PR list)
//   GET /repos/{owner}/{repo}/pulls/{number} (a single open PR)
//   GET /repos/{owner}/{repo}/installation   (the repo's App installation)
//
// Unlike the other cached routes, the PR routes do not get their own state
// table: they absorb into (and rebuild from) the EXISTING pull_requests +
// pr_labels tables the webhook dispatcher and the GraphQL org-repos fetch
// already maintain. What is new here is
//
//   - the pulls_list_cache marker ("this actor's pull_requests rows hold the
//     repo's COMPLETE open-PR set"), which is what makes serving a LIST from
//     state sound: rows alone cannot prove nothing is missing;
//   - rest-completeness (PRRestComplete): GraphQL-sourced rows lack the
//     REST-only columns and can never be rebuilt as a REST response;
//   - a minimal repos row per absorb: webhook maintenance targets partitions
//     via ActorsForRepo (SELECT DISTINCT actor FROM repos), so an actor that
//     only ever absorbed /pulls responses must still own a repos row or the
//     maintaining webhooks would skip it and the absorbed state would go
//     stale while its marker says fresh.

// PRRestComplete reports whether a pull_requests row carries the REST-only
// fields the cached /pulls routes rebuild from. GraphQL-sourced rows
// (SetRepoPRs; identity-locked selection set) lack them and must be treated
// as misses. node_id and base.sha are always present in REST responses and
// webhook payloads and never in the GraphQL selection, so they are the
// signal; author is required by the rebuild shape.
func PRRestComplete(pr dbgen.PullRequest) bool {
	return pr.NodeID.Valid && pr.NodeID.String != "" &&
		pr.BaseRefOid.Valid && pr.BaseRefOid.String != "" &&
		pr.AuthorLogin.Valid && pr.AuthorLogin.String != ""
}

// ensureRepoRow seeds a minimal repos row for the actor so ActorsForRepo
// includes this partition in webhook maintenance. Never overwrites a real
// org-repos row (INSERT .. DO NOTHING).
func ensureRepoRow(ctx context.Context, q *dbgen.Queries, act, owner, repo string) error {
	return q.InsertRepoIfMissing(ctx, dbgen.InsertRepoIfMissingParams{
		Actor: act, Owner: owner, Name: repo, NameWithOwner: owner + "/" + repo,
	})
}

// ---- Open-PR list (GET /repos/{owner}/{repo}/pulls) ----

// PullsListFresh reports whether the actor's "open-PR list complete" marker
// for the repo is valid. A valid marker means the pull_requests rows ARE the
// repo's complete open-PR set (webhooks maintain them; the TTL bounds missed
// deliveries). A hit refreshes the marker's LRU timestamp only -- never its
// expiry.
func (s *Store) PullsListFresh(ctx context.Context, owner, repo string, now time.Time) (bool, error) {
	act := actor.FromContext(ctx)
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetPullsListMarker(ctx, dbgen.GetPullsListMarkerParams{
		Actor: act, Owner: ownerKey, Repo: repoKey,
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
		LastUsedAt: rfc3339(now), Actor: act, Owner: ownerKey, Repo: repoKey,
	})
	return true, nil
}

// RestPullsList returns the actor's open-PR rows for a repo (newest-created
// first, GitHub's default list order) plus all their labels grouped by PR
// number. owner/repo are matched case-insensitively: rows carry GitHub's
// canonical casing, the request URL may not.
func (s *Store) RestPullsList(ctx context.Context, owner, repo string) ([]dbgen.PullRequest, map[int64][]dbgen.PrLabel, error) {
	act := actor.FromContext(ctx)
	prs, err := s.q.ListOpenPullRequestsByRepoNoCase(ctx, dbgen.ListOpenPullRequestsByRepoNoCaseParams{
		Actor: act, Owner: owner, Repo: repo,
	})
	if err != nil {
		return nil, nil, err
	}
	labels, err := s.q.ListPRLabelsByRepoNoCase(ctx, dbgen.ListPRLabelsByRepoNoCaseParams{
		Actor: act, Owner: owner, Repo: repo,
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

// RestSinglePull returns the actor's row for one OPEN PR plus its labels, or
// ok=false when no open row exists.
func (s *Store) RestSinglePull(ctx context.Context, owner, repo string, number int64) (dbgen.PullRequest, []dbgen.PrLabel, bool, error) {
	act := actor.FromContext(ctx)
	pr, err := s.q.GetOpenPullRequestNoCase(ctx, dbgen.GetOpenPullRequestNoCaseParams{
		Actor: act, Owner: owner, Repo: repo, Number: number,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return dbgen.PullRequest{}, nil, false, nil
	}
	if err != nil {
		return dbgen.PullRequest{}, nil, false, err
	}
	labels, err := s.q.ListPRLabelsNoCase(ctx, dbgen.ListPRLabelsNoCaseParams{
		Actor: act, Owner: owner, Repo: repo, PrNumber: number,
	})
	if err != nil {
		return dbgen.PullRequest{}, nil, false, err
	}
	return pr, labels, true, nil
}

// AbsorbPullsList upserts the PRs of a fetched /pulls list response (and
// their labels) for the current actor. When complete is true -- an unfiltered
// page-1 response that provably holds the WHOLE open set -- it also deletes
// open rows the response no longer contains (PRs closed while unwatched) and
// records the "list complete" marker with the given TTL. A filtered or
// possibly-truncated response absorbs rows only: still useful state, but no
// completeness claim. Rows are written with the response's own (canonical)
// owner/repo casing so they collide with webhook/GraphQL-written rows.
func (s *Store) AbsorbPullsList(ctx context.Context, owner, repo string, prs []dbgen.PullRequest, labelsByPR map[int64][]dbgen.PrLabel, complete bool, now time.Time, ttl time.Duration) error {
	act := actor.FromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := ensureRepoRow(ctx, q, act, owner, repo); err != nil {
		return err
	}
	fetched := make(map[int64]bool, len(prs))
	for _, pr := range prs {
		fetched[pr.Number] = true
		if err := q.UpsertPullRequest(ctx, prToParams(act, pr)); err != nil {
			return err
		}
		if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{
			Actor: act, Owner: pr.Owner, Repo: pr.Repo, PrNumber: pr.Number,
		}); err != nil {
			return err
		}
		for _, l := range labelsByPR[pr.Number] {
			if err := q.InsertPRLabel(ctx, dbgen.InsertPRLabelParams{
				Actor: act, Owner: l.Owner, Repo: l.Repo, PrNumber: l.PrNumber,
				Name: l.Name, Color: l.Color,
			}); err != nil {
				return err
			}
		}
	}
	if complete {
		// Drop open rows the complete response does not contain: they closed
		// (or never existed) upstream. Deleting by each stale row's own
		// stored casing keeps the case-sensitive deletes exact. Any orphaned
		// commit_checks rows are left for the webhook close path / rollups.
		existing, err := q.ListOpenPullRequestsByRepoNoCase(ctx, dbgen.ListOpenPullRequestsByRepoNoCaseParams{
			Actor: act, Owner: owner, Repo: repo,
		})
		if err != nil {
			return err
		}
		for _, row := range existing {
			if fetched[row.Number] {
				continue
			}
			if err := q.DeletePullRequest(ctx, dbgen.DeletePullRequestParams{
				Actor: act, Owner: row.Owner, Repo: row.Repo, Number: row.Number,
			}); err != nil {
				return err
			}
			if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{
				Actor: act, Owner: row.Owner, Repo: row.Repo, PrNumber: row.Number,
			}); err != nil {
				return err
			}
		}
		if err := q.UpsertPullsListMarker(ctx, dbgen.UpsertPullsListMarkerParams{
			Actor: act, Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
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

// AbsorbSinglePull upserts one fetched OPEN PR for the current actor. Unlike
// the COALESCE-ing webhook upsert, the fetched mergeable is authoritative --
// including null ("GitHub is recomputing") -- so it is force-set after the
// upsert: a null answer must keep the single-PR route missing (and
// re-fetching) until GitHub resolves it, never resurrect a stale value.
func (s *Store) AbsorbSinglePull(ctx context.Context, pr dbgen.PullRequest, labels []dbgen.PrLabel) error {
	act := actor.FromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := ensureRepoRow(ctx, q, act, pr.Owner, pr.Repo); err != nil {
		return err
	}
	if err := q.UpsertPullRequest(ctx, prToParams(act, pr)); err != nil {
		return err
	}
	if err := q.SetPRMergeable(ctx, dbgen.SetPRMergeableParams{
		Mergeable: pr.Mergeable,
		Actor:     act, Owner: pr.Owner, Repo: pr.Repo, Number: pr.Number,
	}); err != nil {
		return err
	}
	if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{
		Actor: act, Owner: pr.Owner, Repo: pr.Repo, PrNumber: pr.Number,
	}); err != nil {
		return err
	}
	for _, l := range labels {
		if err := q.InsertPRLabel(ctx, dbgen.InsertPRLabelParams{
			Actor: act, Owner: l.Owner, Repo: l.Repo, PrNumber: l.PrNumber,
			Name: l.Name, Color: l.Color,
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeletePullForActor removes one PR row (and its labels) from the CURRENT
// actor's partition -- the "absorbed a closed PR" cleanup. Other partitions
// are corrected by their own webhooks/misses.
func (s *Store) DeletePullForActor(ctx context.Context, owner, repo string, number int64) error {
	act := actor.FromContext(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if err := q.DeletePRLabels(ctx, dbgen.DeletePRLabelsParams{
		Actor: act, Owner: owner, Repo: repo, PrNumber: number,
	}); err != nil {
		return err
	}
	if err := q.DeletePullRequest(ctx, dbgen.DeletePullRequestParams{
		Actor: act, Owner: owner, Repo: repo, Number: number,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// InvalidatePullsListMarkers drops every actor's "list complete" marker for a
// repo -- the structural-event flush (repository renamed/deleted/etc.), NOT
// something pull_request events do (those maintain rows and leave the marker).
func (s *Store) InvalidatePullsListMarkers(ctx context.Context, owner, repo string) error {
	return s.q.DeletePullsListMarkersByRepo(ctx, dbgen.DeletePullsListMarkersByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// NullPRMergeableForBranchForActors un-resolves mergeable on every open PR
// whose base or head is the pushed branch, for every given actor: GitHub
// recomputes mergeability when either side moves and never webhooks the
// result, so the last-known value is stale the moment the push lands. This
// keeps the single-PR route's known-mergeable gate honest (it misses and
// re-fetches instead of serving the pre-push answer).
func (s *Store) NullPRMergeableForBranchForActors(ctx context.Context, actors []string, owner, repo, branch string) error {
	if branch == "" {
		return nil
	}
	return s.forEachActorTx(ctx, func(q *dbgen.Queries, act string) error {
		return q.NullPRMergeableByBranch(ctx, dbgen.NullPRMergeableByBranchParams{
			Actor: act, Owner: owner, Repo: repo,
			BaseRefName: nullStrOrEmpty(branch), HeadRefName: nullStrOrEmpty(branch),
		})
	}, actors)
}

// nullStrOrEmpty mirrors webhook.nullStr for this package's params.
func nullStrOrEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// ---- Repo installation (GET /repos/{owner}/{repo}/installation) ----

// CachedRepoInstallation is the absorbed state of one repo-installation
// response (App-JWT authed; actor is the verified "app:<id>").
type CachedRepoInstallation struct {
	Owner               string // lowercased
	Repo                string // lowercased
	InstallationID      int64
	AccountLogin        string
	AccountType         string
	RepositorySelection string
	AppID               int64
	AppSlug             string
	TargetType          string
}

// GetCachedRepoInstallation returns the cached installation for the current
// actor, or (zero, false) on a miss. An expired row is a miss.
func (s *Store) GetCachedRepoInstallation(ctx context.Context, owner, repo string, now time.Time) (CachedRepoInstallation, bool, error) {
	act := actor.FromContext(ctx)
	row, err := s.q.GetRepoInstallationCache(ctx, dbgen.GetRepoInstallationCacheParams{
		Actor: act, Owner: owner, Repo: repo,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedRepoInstallation{}, false, nil
	}
	if err != nil {
		return CachedRepoInstallation{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return CachedRepoInstallation{}, false, nil
	}
	_ = s.q.TouchRepoInstallationCache(ctx, dbgen.TouchRepoInstallationCacheParams{
		LastUsedAt: rfc3339(now), Actor: act, Owner: owner, Repo: repo,
	})
	return CachedRepoInstallation{
		Owner: row.Owner, Repo: row.Repo, InstallationID: row.InstallationID,
		AccountLogin: row.AccountLogin, AccountType: row.AccountType,
		RepositorySelection: row.RepositorySelection,
		AppID:               row.AppID, AppSlug: row.AppSlug, TargetType: row.TargetType,
	}, true, nil
}

// PutCachedRepoInstallation stores one repo-installation answer for the
// current actor with the given TTL, then prunes expired + over-cap rows.
func (s *Store) PutCachedRepoInstallation(ctx context.Context, c CachedRepoInstallation, now time.Time, ttl time.Duration) error {
	act := actor.FromContext(ctx)
	if err := s.q.UpsertRepoInstallationCache(ctx, dbgen.UpsertRepoInstallationCacheParams{
		Actor: act, Owner: c.Owner, Repo: c.Repo,
		InstallationID: c.InstallationID, AccountLogin: c.AccountLogin, AccountType: c.AccountType,
		RepositorySelection: c.RepositorySelection,
		AppID:               c.AppID, AppSlug: c.AppSlug, TargetType: c.TargetType,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredRepoInstallationCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneRepoInstallationCacheLRU(ctx, CacheMaxRows)
}

// InvalidateRepoInstallationCache drops every cached repo-installation row
// for an installation, across all actors -- installation and
// installation_repositories events change what the installation covers.
func (s *Store) InvalidateRepoInstallationCache(ctx context.Context, installationID int64) error {
	return s.q.DeleteRepoInstallationCacheByInstallation(ctx, installationID)
}
