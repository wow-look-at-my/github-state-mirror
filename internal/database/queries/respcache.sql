-- ============================================================================
-- Cached-route response state (contents / git commits / installation tokens)
-- ============================================================================
--
-- contents_cache and git_commits_cache are GLOBAL truth (one row per resource);
-- whether a caller may read a row is the reveal layer's job (see the
-- access_grants queries in ghdata.sql). install_token_cache and
-- repo_installations stay keyed by the verified app identity: they cache
-- app-specific answers (a minted credential; which installation covers a
-- repo), not shared GitHub state.

-- ---- contents_cache ----

-- name: GetContentsCache :one
SELECT * FROM contents_cache
WHERE owner = ? AND repo = ? AND path = ? AND ref = ?;

-- name: UpsertContentsCache :exec
INSERT INTO contents_cache (owner, repo, path, ref, kind, name, sha, size, encoding, content, entries, message, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, path, ref) DO UPDATE SET
    kind = excluded.kind,
    name = excluded.name,
    sha = excluded.sha,
    size = excluded.size,
    encoding = excluded.encoding,
    content = excluded.content,
    entries = excluded.entries,
    message = excluded.message,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchContentsCache :exec
UPDATE contents_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND path = ? AND ref = ?;

-- name: DeleteContentsCacheByRepo :exec
DELETE FROM contents_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredContentsCache :exec
DELETE FROM contents_cache WHERE expires_at <= ?;

-- PruneContentsCacheLRU keeps only the most recently used rows. The subquery
-- selects everything beyond the newest `offset` rows by last_used_at; LIMIT -1
-- means "no limit" in SQLite, so OFFSET skips the keepers.
-- name: PruneContentsCacheLRU :exec
DELETE FROM contents_cache WHERE id IN (
    SELECT id FROM contents_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- git_commits_cache ----

-- name: GetGitCommitCache :one
SELECT * FROM git_commits_cache
WHERE owner = ? AND repo = ? AND sha = ?;

-- name: UpsertGitCommitCache :exec
INSERT INTO git_commits_cache (owner, repo, sha, message, author_name, author_email, author_date, committer_name, committer_email, committer_date, tree_sha, parents, fetched_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, sha) DO UPDATE SET
    message = excluded.message,
    author_name = excluded.author_name,
    author_email = excluded.author_email,
    author_date = excluded.author_date,
    committer_name = excluded.committer_name,
    committer_email = excluded.committer_email,
    committer_date = excluded.committer_date,
    tree_sha = excluded.tree_sha,
    parents = excluded.parents,
    fetched_at = excluded.fetched_at,
    last_used_at = excluded.last_used_at;

-- name: TouchGitCommitCache :exec
UPDATE git_commits_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND sha = ?;

-- name: PruneGitCommitsCacheLRU :exec
DELETE FROM git_commits_cache WHERE id IN (
    SELECT id FROM git_commits_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- install_token_cache ----

-- name: GetInstallTokenCache :one
SELECT * FROM install_token_cache
WHERE actor = ? AND installation_id = ? AND body_hash = ?;

-- name: UpsertInstallTokenCache :exec
INSERT INTO install_token_cache (actor, installation_id, body_hash, token, token_expires_at, permissions, repository_selection, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, installation_id, body_hash) DO UPDATE SET
    token = excluded.token,
    token_expires_at = excluded.token_expires_at,
    permissions = excluded.permissions,
    repository_selection = excluded.repository_selection,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: DeleteInstallTokenCacheByInstallation :exec
DELETE FROM install_token_cache WHERE installation_id = ?;

-- name: DeleteExpiredInstallTokenCache :exec
DELETE FROM install_token_cache WHERE expires_at <= ?;

-- name: PruneInstallTokenCacheLRU :exec
DELETE FROM install_token_cache WHERE id IN (
    SELECT id FROM install_token_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- pulls_list_cache (the "open-PR list complete" markers) ----

-- name: GetPullsListMarker :one
SELECT * FROM pulls_list_cache
WHERE owner = ? AND repo = ?;

-- name: UpsertPullsListMarker :exec
INSERT INTO pulls_list_cache (owner, repo, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (owner, repo) DO UPDATE SET
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchPullsListMarker :exec
UPDATE pulls_list_cache SET last_used_at = ?
WHERE owner = ? AND repo = ?;

-- DeletePullsListMarkersByRepo drops the marker on structural events
-- (repository renamed/deleted/etc.), NOT on pull_request events (those
-- maintain rows and leave the marker).
-- name: DeletePullsListMarkersByRepo :exec
DELETE FROM pulls_list_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredPullsListMarkers :exec
DELETE FROM pulls_list_cache WHERE expires_at <= ?;

-- name: PrunePullsListMarkersLRU :exec
DELETE FROM pulls_list_cache WHERE id IN (
    SELECT id FROM pulls_list_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- repo_installation_cache (GET /repos/{owner}/{repo}/installation) ----

-- name: GetRepoInstallationCache :one
SELECT * FROM repo_installation_cache
WHERE actor = ? AND owner = ? AND repo = ?;

-- name: UpsertRepoInstallationCache :exec
INSERT INTO repo_installation_cache (actor, owner, repo, installation_id, account_login, account_type, repository_selection, app_id, app_slug, target_type, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, owner, repo) DO UPDATE SET
    installation_id = excluded.installation_id,
    account_login = excluded.account_login,
    account_type = excluded.account_type,
    repository_selection = excluded.repository_selection,
    app_id = excluded.app_id,
    app_slug = excluded.app_slug,
    target_type = excluded.target_type,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchRepoInstallationCache :exec
UPDATE repo_installation_cache SET last_used_at = ?
WHERE actor = ? AND owner = ? AND repo = ?;

-- name: DeleteRepoInstallationCacheByInstallation :exec
DELETE FROM repo_installation_cache WHERE installation_id = ?;

-- name: DeleteExpiredRepoInstallationCache :exec
DELETE FROM repo_installation_cache WHERE expires_at <= ?;

-- name: PruneRepoInstallationCacheLRU :exec
DELETE FROM repo_installation_cache WHERE id IN (
    SELECT id FROM repo_installation_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);
