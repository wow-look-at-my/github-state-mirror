package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
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
// recently used rows beyond this cap are pruned (along with expired rows). It
// is a variable only so tests can lower it; production uses the default.
var CacheMaxRows int64 = 10000

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
// conservative whole-repo flush a push/repository webhook triggers.
// owner/repo are normalized here so callers can pass payload casing.
func (s *Store) InvalidateContentsCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteContentsCacheByRepo(ctx, dbgen.DeleteContentsCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
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
	return q.UpsertGitCommitCache(ctx, dbgen.UpsertGitCommitCacheParams{
		Owner: c.Owner, Repo: c.Repo, Sha: c.SHA, Message: c.Message,
		AuthorName: c.AuthorName, AuthorEmail: c.AuthorEmail, AuthorDate: c.AuthorDate,
		CommitterName: c.CommitterName, CommitterEmail: c.CommitterEmail, CommitterDate: c.CommitterDate,
		TreeSha: c.TreeSHA, Parents: joinParents(c.Parents),
		FetchedAt: rfc3339(now), LastUsedAt: rfc3339(now),
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

// InvalidateInstallTokenCache drops every cached mint for an installation, and
// the repo-installation rows pointing at it -- an installation/
// installation_repositories webhook means the installation's grants changed
// (or it was suspended/deleted), so cached answers must not keep serving.
func (s *Store) InvalidateInstallTokenCache(ctx context.Context, installationID string) error {
	if err := s.q.DeleteInstallTokenCacheByInstallation(ctx, installationID); err != nil {
		return err
	}
	id, err := strconv.ParseInt(installationID, 10, 64)
	if err != nil {
		return nil // token rows flushed; no numeric id to match repo rows on
	}
	return s.q.DeleteRepoInstallationsByInstallation(ctx, id)
}

// ---- Repo installations (GET /repos/{owner}/{repo}/installation) ----

// repoInstallationTTL backstops missed installation webhooks; installations
// change rarely.
const repoInstallationTTL = 6 * time.Hour

// GetRepoInstallation returns the cached installation id covering a repo for
// one app, or (0, false) on a miss.
func (s *Store) GetRepoInstallation(ctx context.Context, appActor, owner, repo string, now time.Time) (int64, bool, error) {
	row, err := s.q.GetRepoInstallation(ctx, dbgen.GetRepoInstallationParams{
		Actor: appActor, Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return 0, false, nil
	}
	return row.InstallationID, true, nil
}

// PutRepoInstallation stores which installation covers a repo for one app.
func (s *Store) PutRepoInstallation(ctx context.Context, appActor, owner, repo string, installationID int64, now time.Time) error {
	if err := s.q.UpsertRepoInstallation(ctx, dbgen.UpsertRepoInstallationParams{
		Actor: appActor, Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
		InstallationID: installationID,
		FetchedAt:      rfc3339(now), ExpiresAt: rfc3339(now.Add(repoInstallationTTL)),
	}); err != nil {
		return err
	}
	return s.q.DeleteExpiredRepoInstallations(ctx, rfc3339(now))
}
