-- ============================================================================
-- Users
-- ============================================================================

-- name: UpsertUser :exec
INSERT INTO users (login, avatar_url, url)
VALUES (?, ?, ?)
ON CONFLICT (login) DO UPDATE SET
    avatar_url = excluded.avatar_url,
    url = excluded.url;

-- name: GetUser :one
SELECT * FROM users WHERE login = ?;

-- name: GetFirstUser :one
SELECT * FROM users LIMIT 1;

-- ============================================================================
-- Orgs
-- ============================================================================

-- name: UpsertOrg :exec
INSERT INTO orgs (login, avatar_url, url)
VALUES (?, ?, ?)
ON CONFLICT (login) DO UPDATE SET
    avatar_url = excluded.avatar_url,
    url = excluded.url;

-- name: GetOrg :one
SELECT * FROM orgs WHERE login = ?;

-- name: ListOrgs :many
SELECT * FROM orgs ORDER BY login;

-- ============================================================================
-- User Org Memberships
-- ============================================================================

-- name: SetUserOrgMembership :exec
INSERT INTO user_org_memberships (user_login, org_login)
VALUES (?, ?)
ON CONFLICT (user_login, org_login) DO NOTHING;

-- name: DeleteUserOrgMemberships :exec
DELETE FROM user_org_memberships WHERE user_login = ?;

-- name: ListUserOrgMemberships :many
SELECT org_login FROM user_org_memberships WHERE user_login = ?;

-- ============================================================================
-- Repos
-- ============================================================================

-- name: UpsertRepo :exec
INSERT INTO repos (owner, name, name_with_owner, url, is_disabled, is_archived, pushed_at, default_branch, default_branch_status, owner_login, owner_avatar, owner_url)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, name) DO UPDATE SET
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
SELECT * FROM repos WHERE owner = ? AND name = ?;

-- name: ListReposByOwner :many
SELECT * FROM repos WHERE owner = ? ORDER BY name;

-- name: DeleteReposByOwner :exec
DELETE FROM repos WHERE owner = ?;

-- ============================================================================
-- Pull Requests
-- ============================================================================

-- name: UpsertPullRequest :exec
INSERT INTO pull_requests (owner, repo, number, title, url, is_draft, state, created_at, updated_at, additions, deletions, mergeable, author_login, author_avatar, author_url, head_ref_name, base_ref_name, head_ref_oid, review_request_count, last_commit_status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, number) DO UPDATE SET
    title = excluded.title,
    url = excluded.url,
    is_draft = excluded.is_draft,
    state = excluded.state,
    created_at = excluded.created_at,
    updated_at = excluded.updated_at,
    additions = excluded.additions,
    deletions = excluded.deletions,
    mergeable = excluded.mergeable,
    author_login = excluded.author_login,
    author_avatar = excluded.author_avatar,
    author_url = excluded.author_url,
    head_ref_name = excluded.head_ref_name,
    base_ref_name = excluded.base_ref_name,
    head_ref_oid = excluded.head_ref_oid,
    review_request_count = excluded.review_request_count,
    last_commit_status = excluded.last_commit_status;

-- name: GetPullRequest :one
SELECT * FROM pull_requests WHERE owner = ? AND repo = ? AND number = ?;

-- name: ListOpenPullRequestsByRepo :many
SELECT * FROM pull_requests
WHERE owner = ? AND repo = ? AND state = 'OPEN'
ORDER BY number;

-- name: ListOpenPullRequestsByOwner :many
SELECT * FROM pull_requests
WHERE owner = ? AND state = 'OPEN'
ORDER BY repo, number;

-- name: DeletePullRequestsByRepo :exec
DELETE FROM pull_requests WHERE owner = ? AND repo = ?;

-- name: DeletePullRequest :exec
DELETE FROM pull_requests WHERE owner = ? AND repo = ? AND number = ?;

-- ============================================================================
-- PR Labels
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

-- ============================================================================
-- PR Files
-- ============================================================================

-- name: InsertPRFile :exec
INSERT INTO pr_files (owner, repo, pr_number, path, additions, deletions)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, pr_number, path) DO UPDATE SET
    additions = excluded.additions,
    deletions = excluded.deletions;

-- name: DeletePRFiles :exec
DELETE FROM pr_files WHERE owner = ? AND repo = ? AND pr_number = ?;

-- name: ListPRFiles :many
SELECT * FROM pr_files WHERE owner = ? AND repo = ? AND pr_number = ?;

-- ============================================================================
-- Branch Comparisons
-- ============================================================================

-- name: UpsertBranchComparison :exec
INSERT INTO branch_comparisons (owner, repo, base_ref, head_ref, ahead_by, behind_by)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, base_ref, head_ref) DO UPDATE SET
    ahead_by = excluded.ahead_by,
    behind_by = excluded.behind_by;

-- name: GetBranchComparison :one
SELECT * FROM branch_comparisons
WHERE owner = ? AND repo = ? AND base_ref = ? AND head_ref = ?;

-- name: DeleteBranchComparison :exec
DELETE FROM branch_comparisons
WHERE owner = ? AND repo = ? AND base_ref = ? AND head_ref = ?;
