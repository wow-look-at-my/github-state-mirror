package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the cached PR REST routes:
//
//	GET /repos/{owner}/{repo}/pulls          (the open-PR list)
//	GET /repos/{owner}/{repo}/pulls/{number} (a single open PR)
//	GET /repos/{owner}/{repo}/installation   (the repo's App installation)
//
// The PR routes do not get their own state table: they absorb into (and
// rebuild from) the GLOBAL pull_requests + pr_labels tables the webhook
// dispatcher and the GraphQL org sync already maintain. What is here is
//
//   - the pulls_list_cache marker ("the global pull_requests rows hold this
//     repo's COMPLETE open-PR set"), which is what makes serving a LIST from
//     state sound: rows alone cannot prove nothing is missing;
//   - rest-completeness (PRRestComplete): GraphQL-sourced rows lack the
//     REST-only columns and can never be rebuilt as a REST response;
//   - a row-staleness backstop (PRRowFresh): a missed `closed` delivery would
//     otherwise serve a stale open PR forever, so a row untouched for longer
//     than PRRowTTL is treated as a miss and re-fetched.
//
// WHO may read the rebuilt answers is the reveal layer's job (internal/api).

// PRRowTTL is the single-PR staleness backstop: a row whose touched_at is
// older than this is not served by the cached single-PR route (it re-fetches
// instead). Webhooks and absorbs stamp touched_at, so any live PR stays well
// inside the window; only a PR that stopped producing events -- e.g. one whose
// close delivery was missed -- ages out. Variable for tests.
var PRRowTTL = 24 * time.Hour

// PRRestComplete reports whether a pull_requests row carries the REST-only
// fields the cached /pulls routes rebuild from. GraphQL-sourced rows
// (identity-locked selection set) lack them and must be treated as misses.
// node_id and base.sha are always present in REST responses and webhook
// payloads and never in the GraphQL selection, so they are the signal; author
// is required by the rebuild shape.
func PRRestComplete(pr dbgen.PullRequest) bool {
	return pr.NodeID.Valid && pr.NodeID.String != "" &&
		pr.BaseRefOid.Valid && pr.BaseRefOid.String != "" &&
		pr.AuthorLogin.Valid && pr.AuthorLogin.String != ""
}

// PRRowFresh reports whether the row was touched (webhook-applied or absorbed)
// recently enough to serve from the single-PR route. An unparseable/empty
// touched_at reads as stale (fail to a re-fetch, never to stale state).
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

// RestPullsList returns the repo's open-PR rows (newest-created first,
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

// RestSinglePull returns the row for one OPEN PR plus its labels, or ok=false
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
// page-1 response that provably holds the WHOLE open set -- it also deletes
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
	fetched := make(map[int64]bool, len(prs))
	for _, pr := range prs {
		fetched[pr.Number] = true
		if err := upsertPRTx(ctx, q, pr, touched); err != nil {
			return err
		}
		if err := replacePRLabelsTx(ctx, q, pr.Owner, pr.Repo, pr.Number, labelsByPR[pr.Number]); err != nil {
			return err
		}
	}
	if complete {
		// Drop open rows the complete response does not contain: they closed
		// (or never existed) upstream -- unless a racing webhook touched them
		// inside the grace window. Deleting by each stale row's own stored
		// casing keeps the case-sensitive deletes exact. Any orphaned
		// commit_checks rows are left for the webhook close path / rollups.
		cutoff := rfc3339(fetchStart.Add(-reconcileGrace))
		existing, err := q.ListOpenPullRequestsByRepoNoCase(ctx, dbgen.ListOpenPullRequestsByRepoNoCaseParams{
			Owner: owner, Repo: repo,
		})
		if err != nil {
			return err
		}
		for _, row := range existing {
			if fetched[row.Number] || row.TouchedAt >= cutoff {
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

// AbsorbSinglePull upserts one fetched OPEN PR into global truth. Unlike the
// COALESCE-ing webhook upsert, the fetched mergeable is authoritative --
// including null ("GitHub is recomputing") -- so it is force-set after the
// upsert: a null answer must keep the single-PR route missing (and
// re-fetching) until GitHub resolves it, never resurrect a stale value.
func (s *Store) AbsorbSinglePull(ctx context.Context, pr dbgen.PullRequest, labels []dbgen.PrLabel, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)

	if err := upsertPRTx(ctx, q, pr, rfc3339(now)); err != nil {
		return err
	}
	if err := q.SetPRMergeable(ctx, dbgen.SetPRMergeableParams{
		Mergeable: pr.Mergeable,
		Owner:     pr.Owner, Repo: pr.Repo, Number: pr.Number,
	}); err != nil {
		return err
	}
	if err := replacePRLabelsTx(ctx, q, pr.Owner, pr.Repo, pr.Number, labels); err != nil {
		return err
	}
	return tx.Commit()
}

// InvalidatePullsListMarkers drops the repo's "list complete" marker -- the
// structural-event flush (repository renamed/deleted/etc.), NOT something
// pull_request events do (those maintain rows and leave the marker).
func (s *Store) InvalidatePullsListMarkers(ctx context.Context, owner, repo string) error {
	return s.q.DeletePullsListMarkersByRepo(ctx, dbgen.DeletePullsListMarkersByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// ---- Repo installation (GET /repos/{owner}/{repo}/installation) ----

// CachedRepoInstallation is the absorbed state of one repo-installation
// response (App-JWT authed; keyed by the verified "app:<id>").
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

// GetCachedRepoInstallation returns the cached installation for the given app
// actor, or (zero, false) on a miss. An expired row is a miss.
func (s *Store) GetCachedRepoInstallation(ctx context.Context, appActor, owner, repo string, now time.Time) (CachedRepoInstallation, bool, error) {
	row, err := s.q.GetRepoInstallationCache(ctx, dbgen.GetRepoInstallationCacheParams{
		Actor: appActor, Owner: owner, Repo: repo,
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
		LastUsedAt: rfc3339(now), Actor: appActor, Owner: owner, Repo: repo,
	})
	return CachedRepoInstallation{
		Owner: row.Owner, Repo: row.Repo, InstallationID: row.InstallationID,
		AccountLogin: row.AccountLogin, AccountType: row.AccountType,
		RepositorySelection: row.RepositorySelection,
		AppID:               row.AppID, AppSlug: row.AppSlug, TargetType: row.TargetType,
	}, true, nil
}

// PutCachedRepoInstallation stores one repo-installation answer for the given
// app actor with the given TTL, then prunes expired + over-cap rows.
func (s *Store) PutCachedRepoInstallation(ctx context.Context, appActor string, c CachedRepoInstallation, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertRepoInstallationCache(ctx, dbgen.UpsertRepoInstallationCacheParams{
		Actor: appActor, Owner: c.Owner, Repo: c.Repo,
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
// for an installation, across all apps -- installation and
// installation_repositories events change what the installation covers.
func (s *Store) InvalidateRepoInstallationCache(ctx context.Context, installationID int64) error {
	return s.q.DeleteRepoInstallationCacheByInstallation(ctx, installationID)
}
