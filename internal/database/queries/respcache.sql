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

-- ---- commits_list_cache (per-page snapshots for the commits LIST route) ----

-- name: GetCommitsListCache :one
SELECT * FROM commits_list_cache
WHERE owner = ? AND repo = ? AND ref_param = ? AND per_page = ? AND page = ?;

-- name: UpsertCommitsListCache :exec
INSERT INTO commits_list_cache (owner, repo, ref_param, per_page, page, shas, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, ref_param, per_page, page) DO UPDATE SET
    shas = excluded.shas,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchCommitsListCache :exec
UPDATE commits_list_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND ref_param = ? AND per_page = ? AND page = ?;

-- DeleteCommitsListCacheByRepo drops a repo's snapshots -- the push/repository
-- webhook flush (a push moves every ref-relative listing). The absorbed
-- git_commits_cache rows are immutable and stay.
-- name: DeleteCommitsListCacheByRepo :exec
DELETE FROM commits_list_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredCommitsListCache :exec
DELETE FROM commits_list_cache WHERE expires_at <= ?;

-- name: PruneCommitsListCacheLRU :exec
DELETE FROM commits_list_cache WHERE id IN (
    SELECT id FROM commits_list_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- compare_cache (GET /repos/{owner}/{repo}/compare/{basehead}) ----

-- name: GetCompareCache :one
SELECT * FROM compare_cache
WHERE owner = ? AND repo = ? AND basehead = ?;

-- name: UpsertCompareCache :exec
INSERT INTO compare_cache (owner, repo, basehead, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, basehead) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchCompareCache :exec
UPDATE compare_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND basehead = ?;

-- DeleteCompareCacheByRepo drops a repo's compare docs -- the push/repository
-- webhook flush (a push to either side of any basehead can change the
-- comparison, so the whole repo flushes).
-- name: DeleteCompareCacheByRepo :exec
DELETE FROM compare_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredCompareCache :exec
DELETE FROM compare_cache WHERE expires_at <= ?;

-- name: PruneCompareCacheLRU :exec
DELETE FROM compare_cache WHERE id IN (
    SELECT id FROM compare_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- commit_ci_cache (GET /repos/{owner}/{repo}/commits/{ref}/status and
-- ---- GET /repos/{owner}/{repo}/commits/{ref}/check-runs) ----

-- name: GetCommitCICache :one
SELECT * FROM commit_ci_cache
WHERE owner = ? AND repo = ? AND ref = ? AND kind = ?;

-- name: UpsertCommitCICache :exec
INSERT INTO commit_ci_cache (owner, repo, ref, kind, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, ref, kind) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchCommitCICache :exec
UPDATE commit_ci_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND ref = ? AND kind = ?;

-- DeleteCommitCICacheByRepo drops a repo's combined-status and check-runs
-- snapshots together -- the status/check_run/check_suite/push/repository
-- webhook flush (CI state changed somewhere in the repo, or a branch-form
-- ref's tip moved; both kinds share the trigger set, so one whole-repo
-- delete covers them).
-- name: DeleteCommitCICacheByRepo :exec
DELETE FROM commit_ci_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredCommitCICache :exec
DELETE FROM commit_ci_cache WHERE expires_at <= ?;

-- name: PruneCommitCICacheLRU :exec
DELETE FROM commit_ci_cache WHERE id IN (
    SELECT id FROM commit_ci_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
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

-- ---- pull_files_cache (GET /repos/{owner}/{repo}/pulls/{number}/files) ----

-- name: GetPullFilesCache :one
SELECT * FROM pull_files_cache
WHERE owner = ? AND repo = ? AND number = ? AND per_page = ? AND page = ?;

-- name: UpsertPullFilesCache :exec
INSERT INTO pull_files_cache (owner, repo, number, per_page, page, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, number, per_page, page) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchPullFilesCache :exec
UPDATE pull_files_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND number = ? AND per_page = ? AND page = ?;

-- DeletePullFilesCacheByRepo drops a repo's PR-files snapshots -- the
-- push/repository webhook flush (a push may have moved any same-repo PR's
-- head; the belt for missed pull_request deliveries).
-- name: DeletePullFilesCacheByRepo :exec
DELETE FROM pull_files_cache WHERE owner = ? AND repo = ?;

-- DeletePullFilesCacheByPR drops one PR's snapshots -- the pull_request event
-- flush (head pushed/synchronize -- including fork heads whose pushes we
-- never see -- base retargets, reopens).
-- name: DeletePullFilesCacheByPR :exec
DELETE FROM pull_files_cache WHERE owner = ? AND repo = ? AND number = ?;

-- name: DeleteExpiredPullFilesCache :exec
DELETE FROM pull_files_cache WHERE expires_at <= ?;

-- name: PrunePullFilesCacheLRU :exec
DELETE FROM pull_files_cache WHERE id IN (
    SELECT id FROM pull_files_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- closed_pull_cache (closed answers for GET /repos/{owner}/{repo}/pulls/{number}) ----

-- name: GetClosedPullCache :one
SELECT * FROM closed_pull_cache
WHERE owner = ? AND repo = ? AND number = ?;

-- name: UpsertClosedPullCache :exec
INSERT INTO closed_pull_cache (owner, repo, number, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, number) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchClosedPullCache :exec
UPDATE closed_pull_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND number = ?;

-- DeleteClosedPullCacheByRepo drops a repo's closed-PR docs -- the repository
-- webhook flush. A push is deliberately NOT a flush: it cannot mutate a
-- closed PR.
-- name: DeleteClosedPullCacheByRepo :exec
DELETE FROM closed_pull_cache WHERE owner = ? AND repo = ?;

-- DeleteClosedPullCacheByPR drops one PR's closed doc -- the pull_request
-- event flush (reopened/edited/relabeled), and the reopened-race safety
-- after an open absorb.
-- name: DeleteClosedPullCacheByPR :exec
DELETE FROM closed_pull_cache WHERE owner = ? AND repo = ? AND number = ?;

-- name: DeleteExpiredClosedPullCache :exec
DELETE FROM closed_pull_cache WHERE expires_at <= ?;

-- name: PruneClosedPullCacheLRU :exec
DELETE FROM closed_pull_cache WHERE id IN (
    SELECT id FROM closed_pull_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);

-- ---- branches_list_cache (GET /repos/{owner}/{repo}/branches) ----

-- name: GetBranchesListCache :one
SELECT * FROM branches_list_cache
WHERE owner = ? AND repo = ? AND per_page = ? AND page = ?;

-- name: UpsertBranchesListCache :exec
INSERT INTO branches_list_cache (owner, repo, per_page, page, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, per_page, page) DO UPDATE SET
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at;

-- name: TouchBranchesListCache :exec
UPDATE branches_list_cache SET last_used_at = ?
WHERE owner = ? AND repo = ? AND per_page = ? AND page = ?;

-- DeleteBranchesListCacheByRepo drops a repo's branches snapshots -- the
-- push/repository webhook flush (branch create, delete, and tip-move all
-- arrive as pushes).
-- name: DeleteBranchesListCacheByRepo :exec
DELETE FROM branches_list_cache WHERE owner = ? AND repo = ?;

-- name: DeleteExpiredBranchesListCache :exec
DELETE FROM branches_list_cache WHERE expires_at <= ?;

-- name: PruneBranchesListCacheLRU :exec
DELETE FROM branches_list_cache WHERE id IN (
    SELECT id FROM branches_list_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
);
