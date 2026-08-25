package ghdata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the cached branches route: GET /repos/{owner}/{repo}/branches.
// see docs/cache/rest-routes.md and docs/webhooks/dispatch.md

// BranchesCacheTTL bounds a missed push's staleness; it lives here since both the fetch-on-miss path and the tip-apply path share this clock.
const BranchesCacheTTL = 24 * time.Hour

// GetCachedBranchesList returns the cached trimmed branches-page document, or
// ("", false) on a miss (no row, or an expired one). A hit refreshes the
// row's LRU timestamp.
func (s *Store) GetCachedBranchesList(ctx context.Context, owner, repo string, perPage, page int64, now time.Time) (string, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetBranchesListCache(ctx, dbgen.GetBranchesListCacheParams{
		Owner: ownerKey, Repo: repoKey, PerPage: perPage, Page: page,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return "", false, nil
	}
	_ = s.q.TouchBranchesListCache(ctx, dbgen.TouchBranchesListCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, PerPage: perPage, Page: page,
	})
	return row.Doc, true, nil
}

// PutCachedBranchesList records one fetched branches page, then prunes the
// table (expired rows + LRU beyond the cap). owner/repo are normalized here
// so callers can pass URL casing.
func (s *Store) PutCachedBranchesList(ctx context.Context, owner, repo string, perPage, page int64, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertBranchesListCache(ctx, dbgen.UpsertBranchesListCacheParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
		PerPage: perPage, Page: page,
		Doc:       doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredBranchesListCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneBranchesListCacheLRU(ctx, CacheMaxRows)
}

// storedBranchListItem is one entry of a stored branches page; field order is wire order and must match the api package's render exactly.
// see docs/webhooks/dispatch.md
type storedBranchListItem struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
	Protected bool `json:"protected"`
}

// ApplyPushedBranchTip writes a push's own `after` tip into every cached page
// that already lists the branch, reporting false where a page cannot be
// edited into the answer (the caller invalidates instead).
// see docs/webhooks/dispatch.md
func (s *Store) ApplyPushedBranchTip(ctx context.Context, owner, repo, branch, afterSHA string, now time.Time, ttl time.Duration) (bool, error) {
	// The all-zeros oid is valid hex and means "no such ref": a deletion, which
	// removes the entry rather than moving it.
	if branch == "" || !IsFullHexSHA(afterSHA) || strings.Trim(afterSHA, "0") == "" {
		return false, nil
	}
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	rows, err := s.q.ListBranchesListCacheByRepo(ctx, dbgen.ListBranchesListCacheByRepoParams{
		Owner: ownerKey, Repo: repoKey,
	})
	if err != nil {
		return false, err
	}
	applied := false
	for _, row := range rows {
		var items []storedBranchListItem
		if err := json.Unmarshal([]byte(row.Doc), &items); err != nil {
			return false, nil // unreadable page: let the caller drop the repo's pages
		}
		found := false
		for i := range items {
			if items[i].Name != branch {
				continue
			}
			found = true
			items[i].Commit.SHA = afterSHA
		}
		if !found {
			continue // this page does not list the branch; it is unaffected
		}
		patched, err := MarshalCacheDoc(items)
		if err != nil {
			return false, nil
		}
		if err := s.PutCachedBranchesList(ctx, ownerKey, repoKey, row.PerPage, row.Page, string(patched), now, ttl); err != nil {
			return false, err
		}
		applied = true
	}
	return applied, nil
}

// InvalidateBranchesListCache drops every cached branches page for a repo, the last resort behind ApplyPushedBranchTip's rewrite.
// see docs/webhooks/dispatch.md
func (s *Store) InvalidateBranchesListCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteBranchesListCacheByRepo(ctx, dbgen.DeleteBranchesListCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
