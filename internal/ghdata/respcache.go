package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// This file is the storage layer for the cached REST routes (repo contents,
// git commits, installation-token mints, repo installations). It stores the
// STATE a GitHub response contained -- never the raw response bytes -- and the
// API layer rebuilds a trimmed response from it (internal/api/respcache.go).
// contents/git-commits rows are GLOBAL truth (who may read one is the reveal
// layer's job); the installation-token and repo-installation caches stay keyed
// by the verified app identity because their answers are app-specific.

// CacheMaxRows bounds each cached-route table: after every write the least
// recently used rows beyond this cap are pruned (along with expired rows).
// cmd/server sets it from the CACHE_MAX_ROWS env var at startup (default
// 1,000,000); tests lower it directly. It is deliberately ONE knob for every
// table: all but git_commits_cache are TTL-bounded (~24h backstop or token
// expiry), so for them the cap is only a runaway safety net -- while
// git_commits_cache (immutable rows, no TTL) is the one table that actually
// grows to the ceiling, evicting its oldest-accessed row on every absorb once
// pinned there (and degrading any commits-list snapshot naming the evicted
// sha into a miss).
var CacheMaxRows int64 = 1_000_000

// rfc3339 formats a time in the fixed-width UTC RFC3339 form used across the
// schema. Fixed width means lexicographic comparison in SQL (the expired-row
// prunes) matches chronological order.
func rfc3339(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// NormalizeRepoKey lowercases an owner or repo name for cache keying. GitHub
// treats owners/repos case-insensitively in URLs, and webhook payloads carry
// the canonical casing, so both sides must fold to one key or invalidation
// could miss rows written under a differently-cased request URL.
func NormalizeRepoKey(s string) string { return strings.ToLower(s) }

// ---- Contents (GET /repos/{owner}/{repo}/contents/{path}) ----

// Contents cache row kinds.
const (
	ContentsKindFile    = "file"
	ContentsKindDir     = "dir"
	ContentsKindMissing = "missing" // a cached 404
)

// CachedContents is the absorbed state of one contents response.
type CachedContents struct {
	Owner    string // lowercased
	Repo     string // lowercased
	Path     string
	Ref      string
	Kind     string // ContentsKind*
	Name     string
	SHA      string
	Size     int64
	Encoding string
	Content  string // base64 exactly as GitHub sent it (incl. line breaks)
	Entries  string // dir listings: JSON array of trimmed entries
	Message  string // missing: GitHub's 404 message
}

// GetCachedContents returns the cached contents state, or (zero, false) on a
// miss. An expired row is a miss (deleted lazily by the next write's prune). A
// hit refreshes the row's LRU timestamp. Callers must have passed the reveal
// check before serving this.
func (s *Store) GetCachedContents(ctx context.Context, owner, repo, path, ref string, now time.Time) (CachedContents, bool, error) {
	row, err := s.q.GetContentsCache(ctx, dbgen.GetContentsCacheParams{
		Owner: owner, Repo: repo, Path: path, Ref: ref,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedContents{}, false, nil
	}
	if err != nil {
		return CachedContents{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return CachedContents{}, false, nil
	}
	// Best-effort LRU touch; a failure must not fail the read.
	_ = s.q.TouchContentsCache(ctx, dbgen.TouchContentsCacheParams{
		LastUsedAt: rfc3339(now), Owner: owner, Repo: repo, Path: path, Ref: ref,
	})
	return CachedContents{
		Owner: row.Owner, Repo: row.Repo, Path: row.Path, Ref: row.Ref,
		Kind: row.Kind, Name: row.Name, SHA: row.Sha, Size: row.Size,
		Encoding: row.Encoding, Content: row.Content, Entries: row.Entries, Message: row.Message,
	}, true, nil
}

// PutCachedContents stores absorbed contents state with the given TTL, then
// prunes expired + over-cap rows.
func (s *Store) PutCachedContents(ctx context.Context, c CachedContents, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertContentsCache(ctx, dbgen.UpsertContentsCacheParams{
		Owner: c.Owner, Repo: c.Repo, Path: c.Path, Ref: c.Ref,
		Kind: c.Kind, Name: c.Name, Sha: c.SHA, Size: c.Size,
		Encoding: c.Encoding, Content: c.Content, Entries: c.Entries, Message: c.Message,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredContentsCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneContentsCacheLRU(ctx, CacheMaxRows)
}

// InvalidateContentsCache drops every cached contents row for a repo -- the
// conservative whole-repo flush a repository webhook (or a push whose ref /
// default branch is unknown) triggers. owner/repo are normalized here so
// callers can pass payload casing.
func (s *Store) InvalidateContentsCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteContentsCacheByRepo(ctx, dbgen.DeleteContentsCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidateContentsForRef drops one requested ref spelling's contents rows
// (ref "" = the default-branch rows) -- the per-ref push flush. A push only
// moves the pushed ref's answers, so other refs' rows survive. owner/repo
// are normalized here so callers can pass payload casing; ref is matched
// verbatim, exactly as rows are keyed.
func (s *Store) InvalidateContentsForRef(ctx context.Context, owner, repo, ref string) error {
	return s.q.DeleteContentsCacheForRef(ctx, dbgen.DeleteContentsCacheForRefParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo), Ref: ref,
	})
}

// ---- Git commits (GET /repos/{owner}/{repo}/git/commits/{sha}) ----

// CachedGitCommit is the absorbed state of one git commit. Rows come from two
// sources -- an upstream fetch, or a push webhook payload -- and both rebuild to
// the same trimmed shape, so the struct is the single source of truth.
type CachedGitCommit struct {
	Owner          string // lowercased
	Repo           string // lowercased
	SHA            string // lowercased full hex
	Message        string
	AuthorName     string
	AuthorEmail    string
	AuthorDate     string // RFC3339 as GitHub reports it
	CommitterName  string
	CommitterEmail string
	CommitterDate  string
	TreeSHA        string
	Parents        []string
}

// GetCachedGitCommit returns the cached commit, or (zero, false) on a miss.
// Commits are immutable: no TTL check. A hit refreshes the LRU timestamp.
func (s *Store) GetCachedGitCommit(ctx context.Context, owner, repo, sha string, now time.Time) (CachedGitCommit, bool, error) {
	row, err := s.q.GetGitCommitCache(ctx, dbgen.GetGitCommitCacheParams{
		Owner: owner, Repo: repo, Sha: sha,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedGitCommit{}, false, nil
	}
	if err != nil {
		return CachedGitCommit{}, false, err
	}
	_ = s.q.TouchGitCommitCache(ctx, dbgen.TouchGitCommitCacheParams{
		LastUsedAt: rfc3339(now), Owner: owner, Repo: repo, Sha: sha,
	})
	return CachedGitCommit{
		Owner: row.Owner, Repo: row.Repo, SHA: row.Sha, Message: row.Message,
		AuthorName: row.AuthorName, AuthorEmail: row.AuthorEmail, AuthorDate: row.AuthorDate,
		CommitterName: row.CommitterName, CommitterEmail: row.CommitterEmail, CommitterDate: row.CommitterDate,
		TreeSHA: row.TreeSha, Parents: splitParents(row.Parents),
	}, true, nil
}

// PutCachedGitCommit stores one commit, then prunes.
func (s *Store) PutCachedGitCommit(ctx context.Context, c CachedGitCommit, now time.Time) error {
	if err := s.upsertGitCommit(ctx, s.q, c, now); err != nil {
		return err
	}
	return s.q.PruneGitCommitsCacheLRU(ctx, CacheMaxRows)
}

// UpsertGitCommits absorbs push-payload commits into global truth in one
// transaction -- the webhook dispatcher's write path. Prunes once afterwards.
func (s *Store) UpsertGitCommits(ctx context.Context, commits []CachedGitCommit, now time.Time) error {
	if len(commits) == 0 {
		return nil
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
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.q.PruneGitCommitsCacheLRU(ctx, CacheMaxRows)
}

func (s *Store) upsertGitCommit(ctx context.Context, q *dbgen.Queries, c CachedGitCommit, now time.Time) error {
	if err := q.UpsertGitCommitCache(ctx, dbgen.UpsertGitCommitCacheParams{
		Owner: c.Owner, Repo: c.Repo, Sha: c.SHA, Message: c.Message,
		AuthorName: c.AuthorName, AuthorEmail: c.AuthorEmail, AuthorDate: c.AuthorDate,
		CommitterName: c.CommitterName, CommitterEmail: c.CommitterEmail, CommitterDate: c.CommitterDate,
		TreeSha: c.TreeSHA, Parents: joinParents(c.Parents),
		FetchedAt: rfc3339(now), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	// INVARIANT: upserting a REAL commit must clear any git_commit_miss_cache
	// marker for its sha, so a sha that was probed before it existed stops
	// answering 404 the moment the commit materializes. EVERY absorb path --
	// the single-commit fetch (PutCachedGitCommit), the push-payload absorb
	// (UpsertGitCommits), and the commits-list/compare absorbs -- funnels
	// through THIS function, so a new absorber can never skip the un-miss.
	// The clear runs on q (the caller's transaction when there is one) so a
	// rolled-back absorb never half-applies. Keys are re-normalized
	// defensively; callers already pass lowercased owner/repo/sha.
	return q.DeleteGitCommitMiss(ctx, dbgen.DeleteGitCommitMissParams{
		Owner: NormalizeRepoKey(c.Owner), Repo: NormalizeRepoKey(c.Repo), Sha: strings.ToLower(c.SHA),
	})
}

// joinParents/splitParents encode a parent-sha list as the compact form stored
// in the parents column. Shas are hex, so a comma join is unambiguous; stored
// as "sha1,sha2" ('' = no parents).
func joinParents(parents []string) string { return strings.Join(parents, ",") }

func splitParents(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	return strings.Split(s, ",")
}

// ---- Installation tokens (POST /app/installations/{id}/access_tokens) ----

// CachedInstallToken is the absorbed state of one token-mint 201 response.
type CachedInstallToken struct {
	InstallationID      string
	BodyHash            string
	Token               string
	TokenExpiresAt      string // GitHub's expires_at, verbatim
	Permissions         string // JSON object; "" when GitHub omitted it
	RepositorySelection string
}

// GetCachedInstallToken returns the cached mint for the given app actor, or
// (zero, false) on a miss. A row past its serve-until expiry is a miss.
func (s *Store) GetCachedInstallToken(ctx context.Context, appActor, installationID, bodyHash string, now time.Time) (CachedInstallToken, bool, error) {
	row, err := s.q.GetInstallTokenCache(ctx, dbgen.GetInstallTokenCacheParams{
		Actor: appActor, InstallationID: installationID, BodyHash: bodyHash,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return CachedInstallToken{}, false, nil
	}
	if err != nil {
		return CachedInstallToken{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return CachedInstallToken{}, false, nil
	}
	return CachedInstallToken{
		InstallationID: row.InstallationID, BodyHash: row.BodyHash,
		Token: row.Token, TokenExpiresAt: row.TokenExpiresAt,
		Permissions: row.Permissions, RepositorySelection: row.RepositorySelection,
	}, true, nil
}

// PutCachedInstallToken stores a mint for the given app actor with the given
// serve-until time, then prunes expired + over-cap rows.
func (s *Store) PutCachedInstallToken(ctx context.Context, appActor string, t CachedInstallToken, now, serveUntil time.Time) error {
	if err := s.q.UpsertInstallTokenCache(ctx, dbgen.UpsertInstallTokenCacheParams{
		Actor: appActor, InstallationID: t.InstallationID, BodyHash: t.BodyHash,
		Token: t.Token, TokenExpiresAt: t.TokenExpiresAt,
		Permissions: t.Permissions, RepositorySelection: t.RepositorySelection,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(serveUntil), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredInstallTokenCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneInstallTokenCacheLRU(ctx, CacheMaxRows)
}

// InvalidateInstallTokenCache drops every cached mint for an installation --
// an installation/installation_repositories webhook means the installation's
// grants changed (or it was suspended/deleted), so cached tokens must not keep
// serving. (The dispatcher also flushes the repo-installation answers via
// InvalidateRepoInstallationCache.)
func (s *Store) InvalidateInstallTokenCache(ctx context.Context, installationID string) error {
	return s.q.DeleteInstallTokenCacheByInstallation(ctx, installationID)
}
