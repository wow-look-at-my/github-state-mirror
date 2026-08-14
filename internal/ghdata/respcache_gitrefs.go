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
// One row per (owner, repo, VERBATIM requested ref). The ref is never
// resolved or canonicalized: "main", "heads/main" and "refs/heads/main" are
// three distinct requests whose answers differ, so each keys its own row and
// the per-ref push flush covers every spelling (refSpellings). status carries
// what was absorbed -- 200 for a real ref, 404 for the absent-ref VERDICT --
// so the route can replay the answer under its own status. WHO may read a row
// is the reveal layer's job (internal/api).

// IsFullHexSHA reports whether s is a full-length (40 or 64) lowercase hex
// object id. It lives in the storage layer because that is where a written
// sha has to be trustworthy -- an all-zeros push tip (a deletion) and a
// truncated id are both rejected here rather than at each caller.
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

// GitRefCacheTTL bounds a stored ref answer. It lives here rather than in the
// route because BOTH writers need it: the fetch-on-miss path and the push
// that applies its own `after` tip (ApplyPushedRefTip) -- an applied tip is
// exactly as fresh as a fetched one, so it gets the same clock.
const GitRefCacheTTL = 24 * time.Hour

// CachedGitRef is one stored ref answer. ReconciledAgainst carries the
// contradicting PR base.sha (if any) that the fetch producing this answer went
// and settled; an empty value on a write LEAVES whatever the row already
// recorded.
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

// GetCachedGitRefChecked is GetCachedGitRef plus the one question that read
// alone cannot answer: does anything else this mirror holds say the stored tip
// is wrong? It returns the row, whether one is live, and -- when a live row is
// CONTRADICTED -- the PR base.sha that contradicts it. A caller must not serve
// a contradicted row; it must fetch, and record the returned sha on the row it
// stores.
//
// The contradiction: an OPEN PR based on this branch carries GitHub's own
// `base.sha` for it, absorbed by a different delivery and a different route
// than the tip. If those two disagree, this mirror is internally inconsistent
// about one branch, and it does not get to pick a side by guessing -- it goes
// and asks. That is the whole mechanism, and it is deliberately not an
// ancestry test: proving WHICH is newer costs the same call as just fetching
// the truth.
//
// Two conditions keep it from firing forever, because a PR's base.sha
// legitimately LAGS its branch (GitHub recomputes it on PR contact, not on
// every base push), so a mismatch is usually lag rather than staleness:
//
//   - the contradicting view must have been absorbed AFTER this row was
//     fetched (touched_at > fetched_at). Evidence older than our own answer
//     was already accounted for by fetching it.
//   - the sha must not be the one a previous fetch already settled
//     (reconciled_against). Re-absorbing the same lagging base.sha is not new
//     evidence.
//
// Together those bound it to one refetch per genuinely new disagreeing sha per
// branch -- roughly one per base-branch move -- while a lost push delivery
// stops being able to wedge the tip at all. docs/cache/stale-tip-repair.md.
func (s *Store) GetCachedGitRefChecked(ctx context.Context, owner, repo, ref string, now time.Time) (CachedGitRef, bool, string, error) {
	c, ok, err := s.GetCachedGitRef(ctx, owner, repo, ref, now)
	if err != nil || !ok {
		return c, ok, "", err
	}
	// Only a real ref answer names a tip. A 404 verdict says the branch does
	// not exist, which an open PR based on it would also contradict -- but
	// that contradiction is already handled: a ref CREATION arrives as a push
	// for that exact ref and drops the verdict.
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

// branchNameFromRefPath reduces a requested ref path to the branch name the PR
// rows key ("heads/main", "refs/heads/main" -> "main"). Anything that is not a
// branch -- a tag, a partial ref -- yields "", because only branches are what
// a PR's base names.
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

// storedGitRefDoc is the stored ref document, declared here so a push can
// REWRITE the tip inside it. Field order is the wire order and must match the
// api package's render exactly -- hit and miss serve identical bytes, and a
// tip applied from a push has to be indistinguishable from a fetched one.
// TestCachedGitRef_PushAppliesTipToEverySpelling (internal/api) pins that by
// byte-comparing an applied answer against the fetched one it replaced.
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
	// The all-zeros oid is valid hex and means "no such ref": a deletion. It
	// must never be WRITTEN as a tip -- that would serve a zero sha as the
	// branch head -- so it falls through to the caller's delete.
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

// ApplyMergedBaseTip writes a merged PR's own statement about its base branch
// into that branch's cached ref row: merging created `merge_commit_sha` ON the
// base branch, so at `mergedAt` that commit WAS the tip.
//
// This exists because the push that states the same thing can be lost, and
// losing it is silent: the row then serves the pre-merge tip for the full TTL,
// and every verdict computed against it is wrong in the direction that stops
// its own repair (docs/cache/stale-tip-repair.md). A merge is delivered TWICE
// -- once as a push, once as pull_request/closed -- so honoring both means one
// lost delivery no longer wedges the tip.
//
// A late delivery must not REGRESS a newer tip (CLAUDE.md: a delivery is a
// view of its own moment), and here the ordering is decidable without a
// fetch: the row records when its content was established, so a merge that
// happened BEFORE that instant says nothing this row does not already
// account for, and is dropped. Only a merge that postdates the row applies.
// Everything else follows ApplyPushedRefTip: 200 rows only, per verbatim
// spelling, an already-current row is a no-op, and a 404-verdict row is left
// to the caller (promoting one would need a node_id no payload carries).
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

// KnownBranchTip reports the tip this mirror currently believes a branch is
// on, read out of the cached ref answers. It is the freshest thing available
// without asking GitHub: a push APPLIES its own `after` into these rows
// (ApplyPushedRefTip), so the tip a delivery states is readable here the
// moment it lands.
//
// name is any spelling of the branch the routes accept ("main", "heads/main",
// "refs/heads/main"); rows key the verbatim requested spelling, so all three
// are consulted and the first live 200 row answers. known=false means exactly
// that -- nobody has asked for this ref, or the answer expired -- and callers
// must treat it as no information rather than as a mismatch.
//
// Tags are deliberately out of scope: an annotated tag's ref object is the
// TAG object, not the commit it points at, so its sha would never equal a
// commit-side tip and every comparison against a tag would read as moved.
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

// InvalidateGitRefForRef drops ONE requested spelling of a ref -- the per-ref
// push flush, and the LAST RESORT for this table (see ApplyPushedRefTip: a
// push that states a real tip updates the row instead). Still the right
// answer for a deletion, a 404-verdict row a creation must clear, and the
// repository events that make the rows wrong rather than stale.
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
