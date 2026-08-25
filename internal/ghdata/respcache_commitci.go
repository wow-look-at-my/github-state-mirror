package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage layer for the cached commit-CI routes (status, check-runs, statuses list).

// CommitCICacheTTL is shared by the fetch-on-miss path and the status delivery that rewrites a document.
const CommitCICacheTTL = 24 * time.Hour

// Commit-CI snapshot kinds (commit_ci_cache.kind).
const (
	// CommitCIKindStatus: GET /repos/{owner}/{repo}/commits/{ref}/status.
	CommitCIKindStatus = "status"
	// CommitCIKindCheckRuns: GET /repos/{owner}/{repo}/commits/{ref}/check-runs.
	CommitCIKindCheckRuns = "check_runs"
	// CommitCIKindStatusesList: GET /repos/{owner}/{repo}/commits/{ref}/statuses.
	CommitCIKindStatusesList = "statuses_list"
)

// CachedCommitCI is one cached commit-CI snapshot. A 403 is never absorbed and never reaches this type.
type CachedCommitCI struct {
	Owner  string // lowercased
	Repo   string // lowercased
	Ref    string // raw ref path segment(s), verbatim, never resolved
	Kind   string // CommitCIKindStatus or CommitCIKindCheckRuns
	Status int    // 200, or 404 (unknown-ref miss marker)
	Doc    string // trimmed document as JSON
}

// GetCachedCommitCI returns the cached snapshot, or (zero, false) on a miss; a hit refreshes its LRU timestamp.
func (s *Store) GetCachedCommitCI(ctx context.Context, owner, repo, ref, kind string, perPage, page int, now time.Time) (CachedCommitCI, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetCommitCICache(ctx, dbgen.GetCommitCICacheParams{
		Owner: ownerKey, Repo: repoKey, Ref: ref, Kind: kind,
		PerPage: int64(perPage), Page: int64(page),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedCommitCI{}, false, nil
	}
	if err != nil {
		return CachedCommitCI{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return CachedCommitCI{}, false, nil
	}
	_ = s.q.TouchCommitCICache(ctx, dbgen.TouchCommitCICacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Ref: ref, Kind: kind,
		PerPage: int64(perPage), Page: int64(page),
	})
	return CachedCommitCI{
		Owner: row.Owner, Repo: row.Repo, Ref: row.Ref, Kind: row.Kind,
		Status: int(row.Status), Doc: row.Doc,
	}, true, nil
}

// PutCachedCommitCI records one fetched snapshot under its exact pagination
// shape, then prunes the table (expired rows + LRU beyond the cap). c must
// carry normalized owner/repo (the API layer's absorb does).
func (s *Store) PutCachedCommitCI(ctx context.Context, c CachedCommitCI, perPage, page int, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertCommitCICache(ctx, dbgen.UpsertCommitCICacheParams{
		Owner: NormalizeRepoKey(c.Owner), Repo: NormalizeRepoKey(c.Repo), Ref: c.Ref, Kind: c.Kind,
		PerPage: int64(perPage), Page: int64(page),
		Status:    int64(c.Status),
		Doc:       c.Doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredCommitCICache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneCommitCICacheLRU(ctx, CacheMaxRows)
}

// InvalidateCommitCICache drops every kind, ref, and page for a repo: the repository flush and the no-refs fallback.
func (s *Store) InvalidateCommitCICache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteCommitCICacheByRepo(ctx, dbgen.DeleteCommitCICacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidateCommitCIForRef drops one verbatim ref spelling's snapshots (all kinds, all pages).
func (s *Store) InvalidateCommitCIForRef(ctx context.Context, owner, repo, ref string) error {
	return s.q.DeleteCommitCICacheForRef(ctx, dbgen.DeleteCommitCICacheForRefParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Ref: ref,
	})
}
