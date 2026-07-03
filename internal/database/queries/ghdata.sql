-- ============================================================================
-- Repos (global truth -- one row per repo)
-- ============================================================================

-- UpsertRepo writes a repo row. visibility is only overwritten when the new
-- value is known (non-empty): the GraphQL org fetch cannot carry visibility,
-- and clobbering a webhook-learned value with '' would silently close the
-- public fast path (or worse, reopen it later from a stale write).
-- name: UpsertRepo :exec
INSERT INTO repos (owner, name, name_with_owner, url, is_disabled, is_archived, visibility, pushed_at, default_branch, default_branch_status, owner_login, owner_avatar, owner_url)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, name) DO UPDATE SET
    name_with_owner = excluded.name_with_owner,
    url = excluded.url,
    is_disabled = excluded.is_disabled,
    is_archived = excluded.is_archived,
    visibility = CASE WHEN excluded.visibility != '' THEN excluded.visibility ELSE repos.visibility END,
    pushed_at = COALESCE(excluded.pushed_at, repos.pushed_at),
    default_branch = COALESCE(excluded.default_branch, repos.default_branch),
    default_branch_status = COALESCE(excluded.default_branch_status, repos.default_branch_status),
    owner_login = COALESCE(excluded.owner_login, repos.owner_login),
    owner_avatar = COALESCE(excluded.owner_avatar, repos.owner_avatar),
    owner_url = COALESCE(excluded.owner_url, repos.owner_url);

-- name: GetRepo :one
SELECT * FROM repos WHERE owner = ? AND name = ?;

-- GetRepoInsensitive looks a repo up by URL-supplied casing. GitHub treats
-- owner/repo case-insensitively in URLs while truth rows keep GitHub's
-- canonical casing, so API-route lookups must fold case.
-- name: GetRepoInsensitive :one
SELECT * FROM repos WHERE owner = ? COLLATE NOCASE AND name = ? COLLATE NOCASE;

-- name: ListReposByOwner :many
SELECT * FROM repos WHERE owner = ? ORDER BY name;

-- ListVisibleReposByOwner returns the owner's repos revealed to one principal:
-- public repos plus repos the principal holds an unexpired grant for.
-- name: ListVisibleReposByOwner :many
SELECT * FROM repos
WHERE repos.owner = ?
  AND (
    repos.visibility = 'public'
    OR EXISTS (
      SELECT 1 FROM access_grants g
      WHERE g.principal = ? AND g.owner = repos.owner AND g.repo = repos.name AND g.expires_at > ?
    )
  )
ORDER BY repos.name;

-- name: DeleteRepo :exec
DELETE FROM repos WHERE owner = ? AND name = ?;

-- name: SetRepoVisibility :exec
UPDATE repos SET visibility = ? WHERE owner = ? AND name = ?;

-- name: SetRepoArchived :exec
UPDATE repos SET is_archived = ? WHERE owner = ? AND name = ?;

-- ============================================================================
-- Pull Requests (global truth -- one row per PR)
-- ============================================================================

-- UpsertPullRequest merges one source's view of a PR into truth. Sources carry
-- different field subsets (GraphQL org fetch, REST list, REST single, webhook
-- payload), so every sometimes-absent column COALESCEs: a NULL argument means
-- "this source does not carry the field", never "clear it". Sources that carry
-- a field with a JSON-null value pass '' (see the schema comment). mergeable
-- and last_commit_status additionally COALESCE because GitHub computes them
-- asynchronously (a payload's null must not clobber a known value); the
-- explicit reset queries below express "GitHub is recomputing this now".
-- name: UpsertPullRequest :exec
INSERT INTO pull_requests (owner, repo, number, title, url, is_draft, state, created_at, updated_at, additions, deletions, mergeable, author_login, author_avatar, author_url, head_ref_name, base_ref_name, head_ref_oid, review_request_count, last_commit_status, node_id, body, auto_merge, mergeable_state, merge_commit_sha, base_sha, head_repo_full_name, touched_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, number) DO UPDATE SET
    title = excluded.title,
    url = excluded.url,
    is_draft = excluded.is_draft,
    state = excluded.state,
    created_at = excluded.created_at,
    updated_at = excluded.updated_at,
    additions = COALESCE(excluded.additions, pull_requests.additions),
    deletions = COALESCE(excluded.deletions, pull_requests.deletions),
    mergeable = COALESCE(excluded.mergeable, pull_requests.mergeable),
    author_login = COALESCE(excluded.author_login, pull_requests.author_login),
    author_avatar = COALESCE(excluded.author_avatar, pull_requests.author_avatar),
    author_url = COALESCE(excluded.author_url, pull_requests.author_url),
    head_ref_name = excluded.head_ref_name,
    base_ref_name = excluded.base_ref_name,
    head_ref_oid = excluded.head_ref_oid,
    review_request_count = COALESCE(excluded.review_request_count, pull_requests.review_request_count),
    last_commit_status = COALESCE(excluded.last_commit_status, pull_requests.last_commit_status),
    node_id = COALESCE(excluded.node_id, pull_requests.node_id),
    body = COALESCE(excluded.body, pull_requests.body),
    auto_merge = COALESCE(excluded.auto_merge, pull_requests.auto_merge),
    mergeable_state = COALESCE(excluded.mergeable_state, pull_requests.mergeable_state),
    merge_commit_sha = COALESCE(excluded.merge_commit_sha, pull_requests.merge_commit_sha),
    base_sha = COALESCE(excluded.base_sha, pull_requests.base_sha),
    head_repo_full_name = COALESCE(excluded.head_repo_full_name, pull_requests.head_repo_full_name),
    touched_at = excluded.touched_at;

-- ResetPRMergeable marks one PR's mergeability as being recomputed by GitHub
-- (head moved: a synchronize event). The stale resolved values must not keep
-- serving -- the /pulls/{n} known-mergeable gate misses on NULL and re-asks
-- GitHub, which is exactly how a poll converges on the fresh answer.
-- name: ResetPRMergeable :exec
UPDATE pull_requests SET mergeable = NULL, mergeable_state = NULL, merge_commit_sha = NULL
WHERE owner = ? AND repo = ? AND number = ?;

-- ResetMergeableByBaseRef marks every open PR targeting a just-pushed base
-- branch as mergeability-unknown (the base moved under them).
-- name: ResetMergeableByBaseRef :exec
UPDATE pull_requests SET mergeable = NULL, mergeable_state = NULL, merge_commit_sha = NULL
WHERE owner = ? AND repo = ? AND base_ref_name = ? AND state = 'OPEN';

-- name: GetPullRequest :one
SELECT * FROM pull_requests WHERE owner = ? AND repo = ? AND number = ?;

-- name: ListOpenPullRequestsByRepo :many
SELECT * FROM pull_requests
WHERE owner = ? AND repo = ? AND state = 'OPEN'
ORDER BY number;

-- ListOpenPullRequestNumbersByRepo feeds the fetch-reconcile: the numbers
-- currently cached open for a repo, with when each row was last touched.
-- name: ListOpenPullRequestNumbersByRepo :many
SELECT number, touched_at FROM pull_requests
WHERE owner = ? AND repo = ? AND state = 'OPEN'
ORDER BY number;

-- name: DeletePullRequest :exec
DELETE FROM pull_requests WHERE owner = ? AND repo = ? AND number = ?;

-- name: DeletePullRequestsByRepo :exec
DELETE FROM pull_requests WHERE owner = ? AND repo = ?;

-- ============================================================================
-- PR Labels (global truth)
-- ============================================================================

-- name: InsertPRLabel :exec
INSERT INTO pr_labels (owner, repo, pr_number, name, color)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, pr_number, name) DO UPDATE SET
    color = excluded.color;

-- name: DeletePRLabels :exec
DELETE FROM pr_labels WHERE owner = ? AND repo = ? AND pr_number = ?;

-- name: ListPRLabels :many
SELECT * FROM pr_labels WHERE owner = ? AND repo = ? AND pr_number = ?;

-- name: DeletePRLabelsByRepo :exec
DELETE FROM pr_labels WHERE owner = ? AND repo = ?;

-- name: SetPRLabelColorByName :exec
UPDATE pr_labels SET color = ?
WHERE owner = ? AND repo = ? AND name = ?;

-- name: DeletePRLabelsByName :exec
DELETE FROM pr_labels WHERE owner = ? AND repo = ? AND name = ?;

-- ============================================================================
-- Commit Check States (CI rollup fed by webhooks; global truth)
-- ============================================================================

-- name: UpsertCommitCheck :exec
INSERT INTO commit_checks (owner, repo, sha, context, state)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, sha, context) DO UPDATE SET
    state = excluded.state;

-- name: ListCommitCheckStates :many
SELECT state FROM commit_checks WHERE owner = ? AND repo = ? AND sha = ?;

-- name: DeleteCommitChecksBySha :exec
DELETE FROM commit_checks WHERE owner = ? AND repo = ? AND sha = ?;

-- name: DeleteCommitChecksByRepo :exec
DELETE FROM commit_checks WHERE owner = ? AND repo = ?;

-- name: SetPRStatusByHeadSha :exec
UPDATE pull_requests SET last_commit_status = ?
WHERE owner = ? AND repo = ? AND head_ref_oid = ?;

-- name: SetRepoPushedAt :exec
UPDATE repos SET pushed_at = ?
WHERE owner = ? AND name = ?;

-- name: SetRepoDefaultBranchStatus :exec
UPDATE repos SET default_branch_status = ?
WHERE owner = ? AND name = ?;

-- ============================================================================
-- Access grants + deny verdicts (the reveal layer)
-- ============================================================================

-- name: UpsertAccessGrant :exec
INSERT INTO access_grants (principal, owner, repo, granted_at, expires_at, source)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (principal, owner, repo) DO UPDATE SET
    granted_at = excluded.granted_at,
    expires_at = excluded.expires_at,
    source = excluded.source;

-- name: GetAccessGrant :one
SELECT * FROM access_grants WHERE principal = ? AND owner = ? AND repo = ?;

-- name: DeleteAccessGrant :exec
DELETE FROM access_grants WHERE principal = ? AND owner = ? AND repo = ?;

-- DeleteListSyncGrants clears one principal's list_sync grants for an owner
-- ahead of a replace-sync. Probe-sourced grants survive (an accessible repo --
-- e.g. an archived one -- may legitimately be absent from the org list).
-- name: DeleteListSyncGrants :exec
DELETE FROM access_grants WHERE principal = ? AND owner = ? AND source = 'list_sync';

-- name: DeleteGrantsByRepo :exec
DELETE FROM access_grants WHERE owner = ? AND repo = ?;

-- name: ListGrantsByPrincipal :many
SELECT * FROM access_grants WHERE principal = ? ORDER BY owner, repo;

-- name: CountGrantsByPrincipal :one
SELECT COUNT(*) FROM access_grants WHERE principal = ? AND expires_at > ?;

-- name: ListGrantPrincipals :many
SELECT principal, COUNT(*) AS grants, MAX(granted_at) AS last_granted
FROM access_grants WHERE expires_at > ?
GROUP BY principal ORDER BY principal;

-- name: DeleteExpiredGrants :exec
DELETE FROM access_grants WHERE expires_at <= ?;

-- name: UpsertDenyVerdict :exec
INSERT INTO deny_cache (principal, resource_kind, resource_key, owner, repo, status, message, denied_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (principal, resource_kind, resource_key) DO UPDATE SET
    owner = excluded.owner,
    repo = excluded.repo,
    status = excluded.status,
    message = excluded.message,
    denied_at = excluded.denied_at,
    expires_at = excluded.expires_at;

-- name: GetDenyVerdict :one
SELECT * FROM deny_cache WHERE principal = ? AND resource_kind = ? AND resource_key = ?;

-- name: DeleteDenialsByPrincipalRepo :exec
DELETE FROM deny_cache WHERE principal = ? AND owner = ? AND repo = ?;

-- name: DeleteDenialsByPrincipalOwner :exec
DELETE FROM deny_cache WHERE principal = ? AND owner = ?;

-- name: DeleteExpiredDenials :exec
DELETE FROM deny_cache WHERE expires_at <= ?;
