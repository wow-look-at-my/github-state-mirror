package ghdata

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage layer for the cached REST routes. Stores the STATE a GitHub

// CacheMaxRows is the per-table row ceiling (see CACHE_MAX_ROWS in CLAUDE.md); only git_commits_cache actually grows to it.
var CacheMaxRows int64 = 1_000_000

// rfc3339 is fixed-width so lexicographic SQL comparison matches chronological order.
func rfc3339(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// MarshalCacheDoc renders a stored document; a hit and a miss must produce identical bytes for the same value.
func MarshalCacheDoc(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Off because GitHub does not escape; a branch named "a&b" must round-trip.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// NormalizeRepoKey folds owner/repo casing so a webhook's canonical casing and a request URL's casing key the same row.
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

// InvalidateContentsCache is the conservative whole-repo flush a repository event (or an unknown-ref push) triggers.
func (s *Store) InvalidateContentsCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteContentsCacheByRepo(ctx, dbgen.DeleteContentsCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}

// InvalidateContentsForRef is the per-ref push flush (ref "" = default branch); other refs' rows survive.
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
	// Every absorb path funnels through here so a real commit always clears its 404 miss marker -- see docs/cache/rest-routes.md.
	return q.DeleteGitCommitMiss(ctx, dbgen.DeleteGitCommitMissParams{
		Owner: NormalizeRepoKey(c.Owner), Repo: NormalizeRepoKey(c.Repo), Sha: strings.ToLower(c.SHA),
	})
}

// joinParents encodes a parent-sha list as "sha1,sha2" ('' = no parents); hex shas make the comma join unambiguous.
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

// InvalidateInstallTokenCache drops every mint for an installation whose grants changed, per an installation event.
func (s *Store) InvalidateInstallTokenCache(ctx context.Context, installationID string) error {
	return s.q.DeleteInstallTokenCacheByInstallation(ctx, installationID)
}

// InvalidateInstallTokenByToken reacts to a 401/403 on the minted token itself, since a consumer App's own permission change reaches no webhook here.
func (s *Store) InvalidateInstallTokenByToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.q.DeleteInstallTokenCacheByToken(ctx, token)
}
