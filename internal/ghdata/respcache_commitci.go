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
//
// One commit_ci_cache row stores the ALREADY-TRIMMED document as one JSON
// blob, keyed by the exact request (owner, repo, raw ref path segment(s),
// kind). The ref is stored VERBATIM -- a branch name (slashes and all), a
// sha, or a tag -- and NEVER resolved, so each spelling is its own snapshot:
// a branch-form row means "that branch's tip at fetch time" and is flushed
// whenever the tip can move. Both kinds share the key shape, TTL, and flush
// triggers exactly, which is why they live in one table with a kind column
// rather than two.
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

// GetCachedCommitCI returns the cached snapshot, or (zero, false) on a miss
// (no row, or an expired one). A hit refreshes the row's LRU timestamp.
func (s *Store) GetCachedCommitCI(ctx context.Context, owner, repo, ref, kind string, now time.Time) (CachedCommitCI, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetCommitCICache(ctx, dbgen.GetCommitCICacheParams{
		Owner: ownerKey, Repo: repoKey, Ref: ref, Kind: kind,
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
	})
	return CachedCommitCI{
		Owner: row.Owner, Repo: row.Repo, Ref: row.Ref, Kind: row.Kind, Doc: row.Doc,
	}, true, nil
}

// PutCachedCommitCI records one fetched snapshot, then prunes the table
// (expired rows + LRU beyond the cap). c must carry normalized owner/repo
// (the API layer's absorb does).
func (s *Store) PutCachedCommitCI(ctx context.Context, c CachedCommitCI, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertCommitCICache(ctx, dbgen.UpsertCommitCICacheParams{
		Owner: NormalizeRepoKey(c.Owner), Repo: NormalizeRepoKey(c.Repo), Ref: c.Ref, Kind: c.Kind,
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

// InvalidateCommitCICache drops every cached commit-CI snapshot (both kinds)
// for a repo -- the status/check_run/check_suite/push/repository webhook
// flush. CI state changed somewhere in the repo (or a branch-form ref's tip
// moved), and a payload cannot resolve which verbatim ref spellings that
// touches, so the whole repo flushes; per-sha precision is deliberately not
// attempted for v1. owner/repo are normalized here so callers can pass
// payload casing.
func (s *Store) InvalidateCommitCICache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteCommitCICacheByRepo(ctx, dbgen.DeleteCommitCICacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
