package ghdata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the cached commits LIST route:
//
//	GET /repos/{owner}/{repo}/commits
//
// Following the absorb-don't-byte-cache doctrine, the listed commits
// themselves live in the SAME global git_commits_cache rows the single
// git-commit route and push payloads maintain (respcache.go). What is here is
// only the per-query ORDERING/COMPLETENESS proof: a commits_list_cache
// snapshot holding the response's shas in order, keyed by the exact modeled
// query shape (owner, repo, ref_param, per_page, page) -- the same rows +
// marker split the pulls list uses. A snapshot whose commits were LRU-pruned
// out of git_commits_cache degrades to a miss (self-healing) rather than
// serving a hole. WHO may read a rebuilt list is the reveal layer's job
// (internal/api).

// GetCachedCommitsList returns the commits of one cached list page in
// response order, or (nil, false) on a miss: no snapshot, an expired
// snapshot, an unreadable sha array, or any listed sha no longer resolving in
// git_commits_cache. A hit refreshes the snapshot's LRU timestamp and each
// commit row's (via GetCachedGitCommit), so a list-served commit earns LRU
// lifetime like a single-read one and the pair stays resident together.
func (s *Store) GetCachedCommitsList(ctx context.Context, owner, repo, refParam string, perPage, page int, now time.Time) ([]CachedGitCommit, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetCommitsListCache(ctx, dbgen.GetCommitsListCacheParams{
		Owner: ownerKey, Repo: repoKey, RefParam: refParam,
		PerPage: int64(perPage), Page: int64(page),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return nil, false, nil
	}
	var shas []string
	if err := json.Unmarshal([]byte(row.Shas), &shas); err != nil {
		return nil, false, nil // unreadable snapshot: a miss, replaced on absorb
	}
	commits := make([]CachedGitCommit, 0, len(shas))
	for _, sha := range shas {
		c, ok, err := s.GetCachedGitCommit(ctx, ownerKey, repoKey, sha, now)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil // a pruned commit row degrades the snapshot to a miss
		}
		commits = append(commits, c)
	}
	_ = s.q.TouchCommitsListCache(ctx, dbgen.TouchCommitsListCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey,
		RefParam: refParam, PerPage: int64(perPage), Page: int64(page),
	})
	return commits, true, nil
}

// PutCachedCommitsList absorbs one fetched list page: every listed commit is
// upserted into the global git_commits_cache and the page's ordered sha
// snapshot is recorded, all in one transaction, then both tables are pruned
// (expired snapshots + LRU beyond the cap). commits must carry normalized
// owner/repo and lowercased shas (the API layer's absorb does).
func (s *Store) PutCachedCommitsList(ctx context.Context, owner, repo, refParam string, perPage, page int, commits []CachedGitCommit, now time.Time, ttl time.Duration) error {
	shas := make([]string, 0, len(commits))
	for _, c := range commits {
		shas = append(shas, c.SHA)
	}
	encoded, err := json.Marshal(shas)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	for _, c := range commits {
		if err := s.upsertGitCommit(ctx, q, c, now); err != nil {
			return err
		}
	}
	if err := q.UpsertCommitsListCache(ctx, dbgen.UpsertCommitsListCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), RefParam: refParam,
		PerPage: int64(perPage), Page: int64(page), Shas: string(encoded),
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if err := s.q.DeleteExpiredCommitsListCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	if err := s.q.PruneCommitsListCacheLRU(ctx, CacheMaxRows); err != nil {
		return err
	}
	return s.q.PruneGitCommitsCacheLRU(ctx, CacheMaxRows)
}

// InvalidateCommitsListCache drops every cached commits-list snapshot for a
// repo -- the repository webhook flush, and the fallback when a push
// payload's ref (or the repo's default branch, which owns the empty-ref
// rows) is unknown. The absorbed git_commits_cache rows are immutable and
// stay. owner/repo are normalized here so callers can pass payload casing.
func (s *Store) InvalidateCommitsListCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteCommitsListCacheByRepo(ctx, dbgen.DeleteCommitsListCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidateCommitsListForRef drops one requested ref spelling's snapshots
// (refParam "" = the default-branch listing) -- the per-ref push flush. A
// push only moves the pushed ref's listings, so other refs' snapshots (and
// the immutable git_commits_cache rows) survive. owner/repo are normalized
// here so callers can pass payload casing; refParam is matched verbatim,
// exactly as snapshots are keyed.
func (s *Store) InvalidateCommitsListForRef(ctx context.Context, owner, repo, refParam string) error {
	return s.q.DeleteCommitsListCacheForRef(ctx, dbgen.DeleteCommitsListCacheForRefParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), RefParam: refParam,
	})
}
