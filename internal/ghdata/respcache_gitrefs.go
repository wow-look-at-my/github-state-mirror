package ghdata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the cached git-ref lookup:
//
//	GET /repos/{owner}/{repo}/git/ref/{ref}
//
// see docs/cache/rest-routes.md

// IsFullHexSHA reports whether s is a full-length (40 or 64) lowercase hex object id.
// It lives in storage: a truncated id is rejected once here, not at every caller.
func IsFullHexSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// GitRefCacheTTL lives here since both the fetch-on-miss path and ApplyPushedRefTip need the same clock for a fresh answer.
const GitRefCacheTTL = 24 * time.Hour

// CachedGitRef is one stored ref answer. ReconciledAgainst carries the contradicting PR base.sha the fetch settled, if any; an empty write leaves the row's existing value.
type CachedGitRef struct {
	Owner             string
	Repo              string
	Ref               string
	Status            int
	Doc               string
	ReconciledAgainst string
}

// GetCachedGitRef returns the cached ref answer, or ok=false on a miss (no
// row, or an expired one). A hit refreshes the row's LRU timestamp.
func (s *Store) GetCachedGitRef(ctx context.Context, owner, repo, ref string, now time.Time) (CachedGitRef, bool, error) {
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetGitRefCache(ctx, dbgen.GetGitRefCacheParams{Owner: ownerKey, Repo: repoKey, Ref: ref})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedGitRef{}, false, nil
	}
	if err != nil {
		return CachedGitRef{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return CachedGitRef{}, false, nil
	}
	_ = s.q.TouchGitRefCache(ctx, dbgen.TouchGitRefCacheParams{
		LastUsedAt: rfc3339(now), Owner: ownerKey, Repo: repoKey, Ref: ref,
	})
	return CachedGitRef{
		Owner: ownerKey, Repo: repoKey, Ref: row.Ref, Status: int(row.Status), Doc: row.Doc,
		ReconciledAgainst: row.ReconciledAgainst,
	}, true, nil
}

// GetCachedGitRefChecked is GetCachedGitRef plus a contradiction check: it
// also returns a contradicting PR base.sha when a live row disagrees with an
// absorbed open PR's base, since a caller must not serve a contradicted row.
// see docs/cache/stale-tip-repair.md
func (s *Store) GetCachedGitRefChecked(ctx context.Context, owner, repo, ref string, now time.Time) (CachedGitRef, bool, string, error) {
	c, ok, err := s.GetCachedGitRef(ctx, owner, repo, ref, now)
	if err != nil || !ok {
		return c, ok, "", err
	}
	// Only a real ref answer names a tip; a ref creation arrives as a push and drops the 404 verdict instead.
	if c.Status != http.StatusOK {
		return c, true, "", nil
	}
	branch := branchNameFromRefPath(ref)
	if branch == "" {
		return c, true, "", nil
	}
	var doc storedGitRefDoc
	if err := json.Unmarshal([]byte(c.Doc), &doc); err != nil {
		return c, true, "", nil
	}
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetGitRefCache(ctx, dbgen.GetGitRefCacheParams{Owner: ownerKey, Repo: repoKey, Ref: ref})
	if err != nil {
		return c, true, "", nil // the read above already succeeded; this is the row's own metadata
	}
	base, err := s.q.ContradictingPRBaseTip(ctx, dbgen.ContradictingPRBaseTipParams{
		Owner: ownerKey, Repo: repoKey, BaseRefName: sql.NullString{String: branch, Valid: true},
		BaseRefOid: sql.NullString{String: doc.Object.SHA, Valid: true}, TouchedAt: row.FetchedAt,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return c, true, "", nil
	}
	if err != nil {
		return c, true, "", err
	}
	sha := strings.ToLower(base.String)
	if !IsFullHexSHA(sha) || sha == c.ReconciledAgainst {
		return c, true, "", nil
	}
	return c, true, sha, nil
}

// branchNameFromRefPath reduces a ref path to the branch name PR rows key; anything not a branch (a tag, a partial ref) yields "".
func branchNameFromRefPath(ref string) string {
	rest := strings.TrimPrefix(ref, "refs/")
	name, ok := strings.CutPrefix(rest, "heads/")
	if !ok || name == "" {
		return ""
	}
	return name
}

// PutCachedGitRef records one fetched ref answer, then prunes the table
// (expired rows + LRU beyond the cap).
func (s *Store) PutCachedGitRef(ctx context.Context, c CachedGitRef, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertGitRefCache(ctx, dbgen.UpsertGitRefCacheParams{
		Owner: NormalizeRepoKey(c.Owner), Repo: NormalizeRepoKey(c.Repo), Ref: c.Ref,
		Status: int64(c.Status), Doc: c.Doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
		ReconciledAgainst: c.ReconciledAgainst,
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredGitRefCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneGitRefCacheLRU(ctx, CacheMaxRows)
}

// storedGitRefDoc's field order is the wire order and must match the api package's render exactly, so hit, miss, and an applied tip serve identical bytes.
type storedGitRefDoc struct {
	Ref    string `json:"ref"`
	NodeID string `json:"node_id"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// ApplyPushedRefTip writes a push's OWN answer into the cached ref rows: the
// payload states the ref's exact new tip (`after`), so there is nothing to go
// ask GitHub for. This is the apply-don't-invalidate rule (CLAUDE.md,
// docs/webhooks/dispatch.md) on the one route whose whole answer a push
// carries -- deleting the row instead spends a needless GET on the next
// reader and leaves the tip unknown until someone pays for it.
//
// Applies only to an existing 200 row, per spelling. A 404 verdict row is
// LEFT for the caller to delete: promoting it to a real answer would need the
// ref's node_id, which no push payload carries, and inventing one would put a
// fabricated field on the wire. A missing row stays missing -- there is no
// stale answer to correct, and caching a ref nobody asked for is not this
// function's job.
func (s *Store) ApplyPushedRefTip(ctx context.Context, owner, repo, ref, afterSHA string, now time.Time, ttl time.Duration) (bool, error) {
	// An all-zeros oid means a deletion and must never be written as a tip; fall through to the caller's delete.
	if !IsFullHexSHA(afterSHA) || strings.Trim(afterSHA, "0") == "" {
		return false, nil
	}
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetGitRefCache(ctx, dbgen.GetGitRefCacheParams{Owner: ownerKey, Repo: repoKey, Ref: ref})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.Status != http.StatusOK {
		return false, nil // absent-ref verdict: the caller's delete owns it
	}
	var doc storedGitRefDoc
	if err := json.Unmarshal([]byte(row.Doc), &doc); err != nil {
		return false, nil // unreadable row: let the caller drop it
	}
	if doc.Object.SHA == afterSHA {
		return true, nil // already current (a replayed delivery)
	}
	doc.Object.SHA = afterSHA
	patched, err := json.Marshal(doc)
	if err != nil {
		return false, nil
	}
	return true, s.PutCachedGitRef(ctx, CachedGitRef{
		Owner: ownerKey, Repo: repoKey, Ref: row.Ref, Status: http.StatusOK, Doc: string(patched),
	}, now, ttl)
}

// ApplyMergedBaseTip writes a merged PR's own base.sha into that branch's
// cached ref row: a merge that postdates the row's established time applies,
// an earlier one is dropped, so no fetch is needed to decide the order.
// see docs/cache/stale-tip-repair.md
func (s *Store) ApplyMergedBaseTip(ctx context.Context, owner, repo, ref, mergeCommitSHA string, mergedAt, now time.Time, ttl time.Duration) (bool, error) {
	if !IsFullHexSHA(mergeCommitSHA) || strings.Trim(mergeCommitSHA, "0") == "" || mergedAt.IsZero() {
		return false, nil
	}
	ownerKey, repoKey := NormalizeRepoKey(owner), NormalizeRepoKey(repo)
	row, err := s.q.GetGitRefCache(ctx, dbgen.GetGitRefCacheParams{Owner: ownerKey, Repo: repoKey, Ref: ref})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.Status != http.StatusOK {
		return false, nil
	}
	established, perr := time.Parse(time.RFC3339, row.FetchedAt)
	if perr != nil || !mergedAt.After(established) {
		return false, nil // the row is at least as new as this merge
	}
	var doc storedGitRefDoc
	if err := json.Unmarshal([]byte(row.Doc), &doc); err != nil {
		return false, nil
	}
	if doc.Object.SHA == mergeCommitSHA {
		return true, nil
	}
	doc.Object.SHA = mergeCommitSHA
	patched, err := json.Marshal(doc)
	if err != nil {
		return false, nil
	}
	return true, s.PutCachedGitRef(ctx, CachedGitRef{
		Owner: ownerKey, Repo: repoKey, Ref: row.Ref, Status: http.StatusOK, Doc: string(patched),
	}, now, ttl)
}

// KnownBranchTip reports the tip this mirror believes a branch is on, read
// from the cached ref rows a push already applied its own tip into. Tags are
// out of scope: an annotated tag's ref object is the tag, not the commit.
// see docs/cache/stale-tip-repair.md
func (s *Store) KnownBranchTip(ctx context.Context, owner, repo, name string, now time.Time) (string, bool, error) {
	short := strings.TrimPrefix(strings.TrimPrefix(name, "refs/"), "heads/")
	if short == "" || strings.HasPrefix(name, "refs/tags/") || strings.HasPrefix(name, "tags/") {
		return "", false, nil
	}
	for _, ref := range []string{short, "heads/" + short, "refs/heads/" + short} {
		c, ok, err := s.GetCachedGitRef(ctx, owner, repo, ref, now)
		if err != nil {
			return "", false, err
		}
		if !ok || c.Status != http.StatusOK {
			continue
		}
		var doc storedGitRefDoc
		if err := json.Unmarshal([]byte(c.Doc), &doc); err != nil {
			continue
		}
		if IsFullHexSHA(doc.Object.SHA) {
			return doc.Object.SHA, true, nil
		}
	}
	return "", false, nil
}

// InvalidateGitRefForRef drops one spelling of a ref -- the last resort here, for a deletion or a creation clearing a 404 verdict.
func (s *Store) InvalidateGitRefForRef(ctx context.Context, owner, repo, ref string) error {
	return s.q.DeleteGitRefCacheForRef(ctx, dbgen.DeleteGitRefCacheForRefParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Ref: ref,
	})
}

// InvalidateGitRefCache drops every cached ref answer for a repo -- the
// repository-event flush (rename/delete/visibility) and the fallback for a
// push payload that names no usable ref.
func (s *Store) InvalidateGitRefCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteGitRefCacheByRepo(ctx, dbgen.DeleteGitRefCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
