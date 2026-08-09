package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the App-JWT-authed installation lookups:
//
//	GET /repos/{owner}/{repo}/installation
//	GET /orgs/{org}/installation
//	GET /users/{username}/installation
//
// Deliberately outside the global-truth model: the answer is app-specific, so
// every row is keyed by the verified "app:<id>" that earned it. The owner-level
// questions share this row space under sentinel repo values (internal/api).

// CachedRepoInstallation is the absorbed state of one repo-installation
// response (App-JWT authed; keyed by the verified "app:<id>"). Status is 200
// for a real installation and 404 for the "not installed here" verdict, whose
// only other field is Message.
type CachedRepoInstallation struct {
	Owner               string // lowercased
	Repo                string // lowercased
	Status              int
	Message             string // 404 verdicts only
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
		Owner: row.Owner, Repo: row.Repo,
		Status: int(row.Status), Message: row.Message,
		InstallationID: row.InstallationID,
		AccountLogin:   row.AccountLogin, AccountType: row.AccountType,
		RepositorySelection: row.RepositorySelection,
		AppID:               row.AppID, AppSlug: row.AppSlug, TargetType: row.TargetType,
	}, true, nil
}

// PutCachedRepoInstallation stores one repo-installation answer for the given
// app actor with the given TTL, then prunes expired + over-cap rows.
func (s *Store) PutCachedRepoInstallation(ctx context.Context, appActor string, c CachedRepoInstallation, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertRepoInstallationCache(ctx, dbgen.UpsertRepoInstallationCacheParams{
		Actor: appActor, Owner: c.Owner, Repo: c.Repo,
		Status: int64(c.Status), Message: c.Message,
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

// InvalidateAbsentRepoInstallations drops every cached "not installed here"
// verdict. Those rows carry no installation id, so the by-id flush above
// cannot reach them; an installation (or installation_repositories) delivery
// says some account's coverage just changed, which is exactly the claim a
// verdict makes. It runs across all apps because the delivery only names OUR
// App's installation id -- a verdict recorded for a consumer App is bounded by
// its own short TTL, not by this.
func (s *Store) InvalidateAbsentRepoInstallations(ctx context.Context) error {
	return s.q.DeleteAbsentRepoInstallationCache(ctx)
}
