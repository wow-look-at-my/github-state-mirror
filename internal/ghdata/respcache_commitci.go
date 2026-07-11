package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the cached commit-CI routes:
//
//	GET /repos/{owner}/{repo}/commits/{ref}/status      (kind "status")
//	GET /repos/{owner}/{repo}/commits/{ref}/check-runs  (kind "check_runs")
//	GET /repos/{owner}/{repo}/commits/{ref}/statuses    (kind "statuses_list")
//
// One commit_ci_cache row stores the ALREADY-TRIMMED document as one JSON
// blob, keyed by the exact request (owner, repo, raw ref path segment(s),
// kind, per_page, page) -- pagination joined the key in round 2 so the
// paginated forms can be modeled; a param-less request uses the defaults
// per_page=30, page=1. The ref is stored VERBATIM -- a branch name (slashes
// and all), a sha, or a tag -- and NEVER resolved, so each spelling is its
// own snapshot: a branch-form row means "that branch's tip at fetch time"
// and is flushed whenever the tip can move. All kinds share the key shape,
// TTL, and flush triggers exactly, which is why they live in one table with
// a kind column rather than three.
//
// These rows deliberately do NOT read or write the commit_checks truth table:
// its normalized per-context rows are lossy against these responses (no
// timestamps, no descriptions, no run ids), so the snapshot is kept whole.
// Unifying the two representations is possible future work. WHO may read a
// cached row is the reveal layer's job (internal/api).

// Commit-CI snapshot kinds (commit_ci_cache.kind).
const (
	// CommitCIKindStatus is the combined commit status
	// (GET /repos/{owner}/{repo}/commits/{ref}/status).
	CommitCIKindStatus = "status"
	// CommitCIKindCheckRuns is the check-runs listing
	// (GET /repos/{owner}/{repo}/commits/{ref}/check-runs).
	CommitCIKindCheckRuns = "check_runs"
	// CommitCIKindStatusesList is the raw statuses LIST
	// (GET /repos/{owner}/{repo}/commits/{ref}/statuses; added round 2).
	CommitCIKindStatusesList = "statuses_list"
)

// CachedCommitCI is one cached commit-CI snapshot: the trimmed document
// exactly as the API layer will serve it.
type CachedCommitCI struct {
	Owner string // lowercased
	Repo  string // lowercased
	Ref   string // raw ref path segment(s), verbatim, never resolved
	Kind  string // CommitCIKindStatus or CommitCIKindCheckRuns
	Doc   string // trimmed document as JSON
}

// GetCachedCommitCI returns the cached snapshot for one exact pagination
// shape, or (zero, false) on a miss (no row, or an expired one). A hit
// refreshes the row's LRU timestamp.
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
		Owner: row.Owner, Repo: row.Repo, Ref: row.Ref, Kind: row.Kind, Doc: row.Doc,
	}, true, nil
}

// PutCachedCommitCI records one fetched snapshot under its exact pagination
// shape, then prunes the table (expired rows + LRU beyond the cap). c must
// carry normalized owner/repo (the API layer's absorb does).
func (s *Store) PutCachedCommitCI(ctx context.Context, c CachedCommitCI, perPage, page int, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertCommitCICache(ctx, dbgen.UpsertCommitCICacheParams{
		Owner: NormalizeRepoKey(c.Owner), Repo: NormalizeRepoKey(c.Repo), Ref: c.Ref, Kind: c.Kind,
		PerPage: int64(perPage), Page: int64(page),
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

// InvalidateCommitCICache drops every cached commit-CI snapshot (every kind,
// every ref, every page) for a repo -- the repository webhook flush, and the
// fallback when a push/check payload names no refs at all. owner/repo are
// normalized here so callers can pass payload casing.
func (s *Store) InvalidateCommitCICache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteCommitCICacheByRepo(ctx, dbgen.DeleteCommitCICacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidateCommitCIForRef drops one verbatim ref spelling's snapshots (all
// kinds, all pages) -- the per-ref status/check_run/check_suite/push flush.
// The payload names exactly which spellings moved (the head branch(es) and
// the sha itself), so other refs' snapshots survive. The ref is matched
// VERBATIM, exactly as rows are keyed; owner/repo are normalized here so
// callers can pass payload casing.
func (s *Store) InvalidateCommitCIForRef(ctx context.Context, owner, repo, ref string) error {
	return s.q.DeleteCommitCICacheForRef(ctx, dbgen.DeleteCommitCICacheForRefParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Ref: ref,
	})
}
