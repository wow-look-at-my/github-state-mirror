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

// This file is the storage layer for the cached branches route:
//
//	GET /repos/{owner}/{repo}/branches
//
// A branches_list_cache row stores the ALREADY-TRIMMED branches array as one
// JSON blob, keyed by the exact request (owner, repo, per_page, page) -- one
// self-contained answer per page, like the compare doc. A listing moves
// whenever any branch is created, deleted, or its tip advances, all of which
// arrive as push events: a tip-move is APPLIED into the stored pages from the
// push's own `after` (ApplyPushedBranchTip), and only the membership changes
// fall back to flushing the repo's snapshots. expires_at is the 24h TTL
// backstop for missed deliveries. WHO may read a cached page is the reveal
// layer's job (internal/api).

// BranchesCacheTTL bounds how long a MISSED push delivery could leave a stale
// listing being served. It lives here rather than in the route because BOTH
// writers need it: the fetch-on-miss path and the push that applies its own
// `after` tip -- an applied tip is exactly as fresh as a fetched one, so it
// gets the same clock.
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

// storedBranchListItem is one entry of a stored branches page, declared here
// so a push can rewrite the tip inside it. Field order is wire order and must
// match the api package's render exactly -- hit and miss serve identical
// bytes, so a tip applied from a push has to be indistinguishable from a
// fetched one. TestCachedBranchesList_PushAppliesTip (internal/api) pins that
// by byte-comparing an applied page against the fetched one it replaced.
type storedBranchListItem struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
	Protected bool `json:"protected"`
}

// ApplyPushedBranchTip writes a push's own `after` tip into every cached page
// that already lists the branch. A tip-move changes exactly one entry's sha
// and nothing else about the listing -- not its membership, not its ordering
// (GitHub sorts by name) -- and the payload states that sha, so there is
// nothing to go ask GitHub for. This is the apply-don't-invalidate rule
// (CLAUDE.md, docs/webhooks/dispatch.md) on the route pr-minder's fork-point
// detection lists per repo, where the alternative is re-listing every branch
// of a repo after every push to it.
//
// Reports false -- the caller invalidates instead -- for the pushes that move
// page MEMBERSHIP rather than one tip, which the pages cannot be edited into:
// a deletion (all-zeros tip), a creation (no page lists the branch yet), and
// an unreadable page. A repo with no cached pages also reports false; there is
// no stale answer to correct, and the caller's flush is then a no-op.
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

// InvalidateBranchesListCache drops every cached branches page for a repo --
// the LAST RESORT for this table (see ApplyPushedBranchTip: a push that only
// moves a listed branch's tip rewrites the pages instead). Still the right
// answer for a create, a delete, and the repository events that make the
// pages wrong rather than stale. owner/repo are normalized here so callers can
// pass payload casing.
func (s *Store) InvalidateBranchesListCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteBranchesListCacheByRepo(ctx, dbgen.DeleteBranchesListCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
