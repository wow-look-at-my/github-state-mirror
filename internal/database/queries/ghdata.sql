-- ============================================================================
-- Users
-- ============================================================================

-- name: UpsertUser :exec
INSERT INTO users (actor, login, avatar_url, url)
VALUES (?, ?, ?, ?)
ON CONFLICT (actor, login) DO UPDATE SET
    avatar_url = excluded.avatar_url,
    url = excluded.url;

-- name: GetUser :one
SELECT * FROM users WHERE actor = ? AND login = ?;

-- name: GetFirstUser :one
SELECT * FROM users WHERE actor = ? LIMIT 1;

-- ============================================================================
-- Orgs
-- ============================================================================

-- name: UpsertOrg :exec
INSERT INTO orgs (actor, login, avatar_url, url)
VALUES (?, ?, ?, ?)
ON CONFLICT (actor, login) DO UPDATE SET
    avatar_url = excluded.avatar_url,
    url = excluded.url;

-- name: GetOrg :one
SELECT * FROM orgs WHERE actor = ? AND login = ?;

-- name: ListOrgs :many
SELECT * FROM orgs WHERE actor = ? ORDER BY login;

-- ============================================================================
-- User Org Memberships
-- ============================================================================

-- name: SetUserOrgMembership :exec
INSERT INTO user_org_memberships (actor, user_login, org_login)
VALUES (?, ?, ?)
ON CONFLICT (actor, user_login, org_login) DO NOTHING;

-- name: DeleteUserOrgMemberships :exec
DELETE FROM user_org_memberships WHERE actor = ? AND user_login = ?;

-- name: ListUserOrgMemberships :many
SELECT org_login FROM user_org_memberships WHERE actor = ? AND user_login = ?;

-- ============================================================================
-- Repos
-- ============================================================================

-- name: UpsertRepo :exec
INSERT INTO repos (actor, owner, name, name_with_owner, url, is_disabled, is_archived, pushed_at, default_branch, default_branch_status, owner_login, owner_avatar, owner_url)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, owner, name) DO UPDATE SET
    name_with_owner = excluded.name_with_owner,
    url = excluded.url,
    is_disabled = excluded.is_disabled,
    is_archived = excluded.is_archived,
    pushed_at = excluded.pushed_at,
    default_branch = excluded.default_branch,
    default_branch_status = excluded.default_branch_status,
    owner_login = excluded.owner_login,
    owner_avatar = excluded.owner_avatar,
    owner_url = excluded.owner_url;

-- name: GetRepo :one
SELECT * FROM repos WHERE actor = ? AND owner = ? AND name = ?;

-- name: ListReposByOwner :many
SELECT * FROM repos WHERE actor = ? AND owner = ? ORDER BY name;

-- name: DeleteReposByOwner :exec
DELETE FROM repos WHERE actor = ? AND owner = ?;

-- ListActorsForRepo matches owner/name case-insensitively: GitHub treats both
-- case-insensitively, and repos rows can carry request-URL casing (the cached
-- /pulls routes ensure a row on absorb) while webhook payloads carry canonical
-- casing -- a casing mismatch must not hide a partition from maintenance.
-- name: ListActorsForRepo :many
SELECT DISTINCT actor FROM repos WHERE owner = ? COLLATE NOCASE AND name = ? COLLATE NOCASE;

-- InsertRepoIfMissing seeds a minimal repos row so ActorsForRepo includes the
-- actor (webhook maintenance targets partitions via repos rows). It never
-- overwrites an existing row: a real org-repos fetch owns those fields.
-- name: InsertRepoIfMissing :exec
INSERT INTO repos (actor, owner, name, name_with_owner, url)
VALUES (?, ?, ?, ?, '')
ON CONFLICT (actor, owner, name) DO NOTHING;

-- ============================================================================
-- Pull Requests
-- ============================================================================

-- The REST-only columns (node_id .. merge_commit_sha) COALESCE against the
-- existing row so a GraphQL-shaped upsert (which cannot know them) never wipes
-- values a REST/webhook write recorded -- with two exceptions that are real
-- state, not "unknown": body and auto_merge_method overwrite whenever the
-- source knows the REST fields at all (node_id present), because a null body
-- and a disarmed auto-merge are meaningful values a COALESCE would resurrect.
-- That conditional is expressed as CASE WHEN excluded.node_id IS NULL (the
-- GraphQL-source signature) THEN keep ELSE take END.
-- name: UpsertPullRequest :exec
INSERT INTO pull_requests (actor, owner, repo, number, title, url, is_draft, state, created_at, updated_at, additions, deletions, mergeable, author_login, author_avatar, author_url, head_ref_name, base_ref_name, head_ref_oid, review_request_count, last_commit_status, node_id, body, author_type, base_ref_oid, head_repo_full_name, auto_merge_method, merge_commit_sha)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, owner, repo, number) DO UPDATE SET
    title = excluded.title,
    url = excluded.url,
    is_draft = excluded.is_draft,
    state = excluded.state,
    created_at = excluded.created_at,
    updated_at = excluded.updated_at,
    additions = excluded.additions,
    deletions = excluded.deletions,
    mergeable = COALESCE(excluded.mergeable, pull_requests.mergeable),
    author_login = excluded.author_login,
    author_avatar = excluded.author_avatar,
    author_url = excluded.author_url,
    head_ref_name = excluded.head_ref_name,
    base_ref_name = excluded.base_ref_name,
    head_ref_oid = excluded.head_ref_oid,
    review_request_count = excluded.review_request_count,
    last_commit_status = COALESCE(excluded.last_commit_status, pull_requests.last_commit_status),
    node_id = COALESCE(excluded.node_id, pull_requests.node_id),
    body = CASE WHEN excluded.node_id IS NULL THEN pull_requests.body ELSE excluded.body END,
    author_type = COALESCE(excluded.author_type, pull_requests.author_type),
    base_ref_oid = COALESCE(excluded.base_ref_oid, pull_requests.base_ref_oid),
    head_repo_full_name = COALESCE(excluded.head_repo_full_name, pull_requests.head_repo_full_name),
    auto_merge_method = CASE WHEN excluded.node_id IS NULL THEN pull_requests.auto_merge_method ELSE excluded.auto_merge_method END,
    merge_commit_sha = CASE WHEN excluded.node_id IS NULL THEN pull_requests.merge_commit_sha ELSE excluded.merge_commit_sha END;

-- name: GetPullRequest :one
SELECT * FROM pull_requests WHERE actor = ? AND owner = ? AND repo = ? AND number = ?;

-- GetOpenPullRequestNoCase is the cached single-PR route's read: owner/repo
-- matched case-insensitively (rows carry GitHub's canonical casing; the
-- request URL may not), open PRs only (the cache never retains closed ones).
-- name: GetOpenPullRequestNoCase :one
SELECT * FROM pull_requests
WHERE actor = ? AND owner = ? COLLATE NOCASE AND repo = ? COLLATE NOCASE AND number = ? AND state = 'OPEN';

-- ListOpenPullRequestsByRepoNoCase is the cached list route's read. Ordered
-- newest-created first to match GitHub's default list-pulls sort.
-- name: ListOpenPullRequestsByRepoNoCase :many
SELECT * FROM pull_requests
WHERE actor = ? AND owner = ? COLLATE NOCASE AND repo = ? COLLATE NOCASE AND state = 'OPEN'
ORDER BY created_at DESC, number DESC;

-- name: ListOpenPullRequestsByRepo :many
SELECT * FROM pull_requests
WHERE actor = ? AND owner = ? AND repo = ? AND state = 'OPEN'
ORDER BY number;

-- name: ListOpenPullRequestsByOwner :many
SELECT * FROM pull_requests
WHERE actor = ? AND owner = ? AND state = 'OPEN'
ORDER BY repo, number;

-- name: DeletePullRequestsByRepo :exec
DELETE FROM pull_requests WHERE actor = ? AND owner = ? AND repo = ?;

-- name: DeletePullRequest :exec
DELETE FROM pull_requests WHERE actor = ? AND owner = ? AND repo = ? AND number = ?;

-- ============================================================================
-- PR Labels
-- ============================================================================

-- name: InsertPRLabel :exec
INSERT INTO pr_labels (actor, owner, repo, pr_number, name, color)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, owner, repo, pr_number, name) DO UPDATE SET
    color = excluded.color;

-- name: DeletePRLabels :exec
DELETE FROM pr_labels WHERE actor = ? AND owner = ? AND repo = ? AND pr_number = ?;

-- name: ListPRLabels :many
SELECT * FROM pr_labels WHERE actor = ? AND owner = ? AND repo = ? AND pr_number = ?;

-- ListPRLabelsByRepoNoCase feeds the cached /pulls list rebuild: all of a
-- repo's PR labels in one query (grouped by pr_number in Go), owner/repo
-- matched case-insensitively like the row reads.
-- name: ListPRLabelsByRepoNoCase :many
SELECT * FROM pr_labels
WHERE actor = ? AND owner = ? COLLATE NOCASE AND repo = ? COLLATE NOCASE
ORDER BY pr_number, name;

-- name: ListPRLabelsNoCase :many
SELECT * FROM pr_labels
WHERE actor = ? AND owner = ? COLLATE NOCASE AND repo = ? COLLATE NOCASE AND pr_number = ?
ORDER BY name;

-- name: DeletePRLabelsByRepo :exec
DELETE FROM pr_labels WHERE actor = ? AND owner = ? AND repo = ?;

-- ============================================================================
-- PR Files
-- ============================================================================

-- name: InsertPRFile :exec
INSERT INTO pr_files (actor, owner, repo, pr_number, path, additions, deletions)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, owner, repo, pr_number, path) DO UPDATE SET
    additions = excluded.additions,
    deletions = excluded.deletions;

-- name: DeletePRFiles :exec
DELETE FROM pr_files WHERE actor = ? AND owner = ? AND repo = ? AND pr_number = ?;

-- name: ListPRFiles :many
SELECT * FROM pr_files WHERE actor = ? AND owner = ? AND repo = ? AND pr_number = ?;

-- ============================================================================
-- Branch Comparisons
-- ============================================================================

-- name: UpsertBranchComparison :exec
INSERT INTO branch_comparisons (actor, owner, repo, base_ref, head_ref, ahead_by, behind_by)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, owner, repo, base_ref, head_ref) DO UPDATE SET
    ahead_by = excluded.ahead_by,
    behind_by = excluded.behind_by;

-- name: GetBranchComparison :one
SELECT * FROM branch_comparisons
WHERE actor = ? AND owner = ? AND repo = ? AND base_ref = ? AND head_ref = ?;

-- name: DeleteBranchComparison :exec
DELETE FROM branch_comparisons
WHERE actor = ? AND owner = ? AND repo = ? AND base_ref = ? AND head_ref = ?;

-- ============================================================================
-- Commit Check States (CI rollup fed by webhooks)
-- ============================================================================

-- name: UpsertCommitCheck :exec
INSERT INTO commit_checks (actor, owner, repo, sha, context, state)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (actor, owner, repo, sha, context) DO UPDATE SET
    state = excluded.state;

-- name: ListCommitCheckStates :many
SELECT state FROM commit_checks WHERE actor = ? AND owner = ? AND repo = ? AND sha = ?;

-- name: DeleteCommitChecksBySha :exec
DELETE FROM commit_checks WHERE actor = ? AND owner = ? AND repo = ? AND sha = ?;

-- name: SetPRStatusByHeadSha :exec
UPDATE pull_requests SET last_commit_status = ?
WHERE actor = ? AND owner = ? AND repo = ? AND head_ref_oid = ?;

-- SetPRMergeable overwrites a PR's stored mergeable with GitHub's freshly
-- fetched answer, INCLUDING null (recomputing). The upsert's COALESCE keeps
-- old values on null payloads; a direct REST read of the PR is authoritative
-- about "currently unresolved", so the cached single-PR route uses this after
-- absorbing to make a null answer miss again until GitHub resolves it.
-- name: SetPRMergeable :exec
UPDATE pull_requests SET mergeable = ?
WHERE actor = ? AND owner = ? AND repo = ? AND number = ?;

-- NullPRMergeableByBranch un-resolves mergeable for every open PR whose base
-- or head is the pushed branch: GitHub recomputes mergeability after either
-- side moves (and emits NO webhook with the result), so the last-known value
-- is stale the moment the push lands. Nulling makes the cached single-PR
-- route's known-mergeable gate miss and re-fetch, mirroring GitHub's own
-- null-while-recomputing behavior.
-- name: NullPRMergeableByBranch :exec
UPDATE pull_requests SET mergeable = NULL
WHERE actor = ? AND owner = ? AND repo = ? AND state = 'OPEN'
  AND (base_ref_name = ? OR head_ref_name = ?);

-- name: SetRepoPushedAt :exec
UPDATE repos SET pushed_at = ?
WHERE actor = ? AND owner = ? AND name = ?;

-- name: SetRepoDefaultBranchStatus :exec
UPDATE repos SET default_branch_status = ?
WHERE actor = ? AND owner = ? AND name = ?;

-- name: SetPRLabelColorByName :exec
UPDATE pr_labels SET color = ?
WHERE actor = ? AND owner = ? AND repo = ? AND name = ?;

-- name: DeletePRLabelsByName :exec
DELETE FROM pr_labels WHERE actor = ? AND owner = ? AND repo = ? AND name = ?;
