package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the cached compare route:
//
//	GET /repos/{owner}/{repo}/compare/{basehead}
//
// A compare_cache row stores the ALREADY-TRIMMED compare document as one JSON
// blob, keyed by the exact request (owner, repo, raw basehead path tail) --
// unlike the commits list there is no rows+snapshot split, because a
// comparison is one self-contained answer, not an ordering over shared rows.
// The compare's commits ARE still upserted into the global git_commits_cache
// on absorb (the same rows the single git-commit route and push payloads
// maintain -- pure synergy; a compare hit never depends on them). A
// comparison depends on both refs' tips, so push/repository webhooks flush
// ALL of a repo's rows; expires_at is the 24h TTL backstop. WHO may read a
// cached comparison is the reveal layer's job (internal/api).

// CachedCompare is one cached comparison: the rendered document exactly as
// the API layer will serve it. BaseRef/HeadRef are the basehead's two sides
// (the API layer splits at the "..." its route guard already requires), kept
// as their own columns so a push to one ref can flush exactly the
// comparisons naming it. Status is the upstream answer the row absorbed: 200
// (a real comparison) or 404 (an expiring unknown-ref miss marker, absorbed
// by the round-2 route work); Doc holds the rendered body either way.
type CachedCompare struct {
	Owner    string // lowercased
	Repo     string // lowercased
	Basehead string // raw base...head path tail, exact
	BaseRef  string // basehead's base side (before the "...")
	HeadRef  string // basehead's head side (after the "...")
	// BaseTipSha is base_commit.sha from the answer GitHub gave: the exact
	// base-side tip this comparison was computed against. Empty when the
	// upstream body did not state one (and on a 404 verdict row).
	BaseTipSha string
	Status     int    // 200, or 404 (unknown-ref miss marker)
	Doc        string // rendered document as JSON (trimmed compare, or the 404 body)
}

// GetCachedCompare returns the cached comparison, or (zero, false) on a miss
// (no row, or an expired one). A hit refreshes the row's LRU timestamp.
func (s *Store) GetCachedCompare(ctx context.Context, owner, repo, basehead string, now time.Time) (CachedCompare, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetCompareCache(ctx, dbgen.GetCompareCacheParams{
		Owner: ownerKey, Repo: repoKey, Basehead: basehead,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedCompare{}, false, nil
	}
	if err != nil {
		return CachedCompare{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return CachedCompare{}, false, nil
	}
	if moved, err := s.baseBranchMoved(ctx, ownerKey, repoKey, row.BaseRef, row.BaseTipSha, now); err != nil {
		return CachedCompare{}, false, err
	} else if moved {
		return CachedCompare{}, false, nil
	}
	_ = s.q.TouchCompareCache(ctx, dbgen.TouchCompareCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Basehead: basehead,
	})
	return CachedCompare{
		Owner: row.Owner, Repo: row.Repo, Basehead: row.Basehead,
		BaseRef: row.BaseRef, HeadRef: row.HeadRef, BaseTipSha: row.BaseTipSha,
		Status: int(row.Status), Doc: row.Doc,
	}, true, nil
}

// baseBranchMoved reports whether the base branch has moved off the tip this
// row was computed against -- a row we can PROVE is stale, whatever happened
// to the flush that should have dropped it.
//
// The flush is still the primary mechanism, and it is not going anywhere.
// This is the belt for the case that actually bit: a `behind_by` answer
// outlives the push that changed it, and the consumer reading it (pr-minder's
// "does this PR need its branch updated?" gate) concludes there is nothing to
// do. Nothing about that failure is self-correcting -- the wrong answer is
// what stops the work that would produce a new one -- so it sits until the
// 24h TTL. GitHub hands us the way out in the answer itself: base_commit.sha
// says which base tip the comparison describes, and git_ref_cache says which
// tip the branch is on NOW, because a push APPLIES its own `after` there
// (ApplyPushedRefTip). Two facts we already hold, compared on read.
//
// Unknown beats guessing in only one direction here. A row with no stated
// base tip, a base side that is a sha (immutable -- nothing to move), or a
// branch whose tip we have never observed all fall through to the existing
// contract: flush, then TTL. Refusing to serve those would not make one of
// them fresher; it would just turn every first comparison into a permanent
// miss. What this rejects is the narrow, provable case: we know the tip, and
// it is not the one in the row.
//
// The HEAD side gets no equivalent check. GitHub states no head tip, and
// deriving one from the last element of `commits` is wrong the moment a
// comparison exceeds GitHub's 250-commit cap -- a silently truncated list
// whose tail is not the head. The head side keeps the flush and the TTL.
func (s *Store) baseBranchMoved(ctx context.Context, owner, repo, baseRef, baseTipSha string, now time.Time) (bool, error) {
	if baseTipSha == "" || IsFullHexSHA(baseRef) {
		return false, nil
	}
	tip, known, err := s.KnownBranchTip(ctx, owner, repo, baseRef, now)
	if err != nil || !known {
		return false, err
	}
	return tip != baseTipSha, nil
}

// PutCachedCompare absorbs one fetched comparison: the compare's commits are
// upserted into the global git_commits_cache (synergy with the single-commit
// and commits-list routes) and the trimmed document is recorded, all in one
// transaction, then both tables are pruned (expired rows + LRU beyond the
// cap). c and commits must carry normalized owner/repo and lowercased shas
// (the API layer's absorb does).
func (s *Store) PutCachedCompare(ctx context.Context, c CachedCompare, commits []CachedGitCommit, now time.Time, ttl time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	for _, commit := range commits {
		if err := s.upsertGitCommit(ctx, q, commit, now); err != nil {
			return err
		}
	}
	if err := q.UpsertCompareCache(ctx, dbgen.UpsertCompareCacheParams{
		Owner: NormalizeRepoKey(c.Owner), Repo: NormalizeRepoKey(c.Repo), Basehead: c.Basehead,
		BaseRef: c.BaseRef, HeadRef: c.HeadRef, Status: int64(c.Status),
		Doc:       c.Doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if err := s.q.DeleteExpiredCompareCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	if err := s.q.PruneCompareCacheLRU(ctx, CacheMaxRows); err != nil {
		return err
	}
	return s.q.PruneGitCommitsCacheLRU(ctx, CacheMaxRows)
}

// InvalidateCompareCache drops every cached comparison for a repo -- the
// repository webhook flush, and the fallback when a push payload's ref is
// unknown (a push to either side of any basehead can change the comparison).
// owner/repo are normalized here so callers can pass payload casing.
func (s *Store) InvalidateCompareCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteCompareCacheByRepo(ctx, dbgen.DeleteCompareCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidateCompareForRef drops every cached comparison naming one ref on
// EITHER side (base_ref or head_ref) -- the per-ref push flush. Comparisons
// not touching the pushed ref survive. The ref is matched verbatim (compare
// rows never key an empty side; the route guard requires both sides
// non-empty); owner/repo are normalized here so callers can pass payload
// casing.
func (s *Store) InvalidateCompareForRef(ctx context.Context, owner, repo, ref string) error {
	return s.q.DeleteCompareCacheForRef(ctx, dbgen.DeleteCompareCacheForRefParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
		BaseRef: ref, HeadRef: ref,
	})
}
