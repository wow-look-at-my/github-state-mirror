-- Schema version: bump this constant in db.go when changing this file.
-- The DB is a cache — on version mismatch, the file gets deleted and recreated.

-- ============================================================================
-- Freshness / Cache Metadata (generic, no GitHub knowledge)
-- ============================================================================

CREATE TABLE schema_version (
    version INTEGER NOT NULL
);

CREATE TABLE cache_metadata (
    actor           TEXT NOT NULL DEFAULT '',
    resource_kind   TEXT NOT NULL,
    resource_key    TEXT NOT NULL,
    last_fetched_at TEXT,           -- RFC3339
    last_changed_at TEXT,           -- RFC3339
    etag            TEXT,
    expires_at      TEXT,           -- RFC3339
    fetch_state     TEXT NOT NULL DEFAULT 'unknown',
    error_message   TEXT,
    retry_after     TEXT,           -- RFC3339
    PRIMARY KEY (actor, resource_kind, resource_key)
);

CREATE TABLE cache_refresh_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    actor           TEXT NOT NULL DEFAULT '',
    resource_kind   TEXT NOT NULL,
    resource_key    TEXT NOT NULL,
    triggered_by    TEXT NOT NULL,
    started_at      TEXT NOT NULL,
    completed_at    TEXT,
    success         INTEGER,
    records_changed INTEGER,
    error_message   TEXT
);

-- ============================================================================
-- GitHub Data Tables
-- ============================================================================

CREATE TABLE users (
    actor       TEXT NOT NULL DEFAULT '',
    login       TEXT NOT NULL,
    avatar_url  TEXT NOT NULL,
    url         TEXT NOT NULL,
    PRIMARY KEY (actor, login)
);

CREATE TABLE orgs (
    actor       TEXT NOT NULL DEFAULT '',
    login       TEXT NOT NULL,
    avatar_url  TEXT,
    url         TEXT,
    PRIMARY KEY (actor, login)
);

CREATE TABLE user_org_memberships (
    actor       TEXT NOT NULL DEFAULT '',
    user_login  TEXT NOT NULL,
    org_login   TEXT NOT NULL,
    PRIMARY KEY (actor, user_login, org_login)
);

CREATE TABLE repos (
    actor                 TEXT NOT NULL DEFAULT '',
    owner                 TEXT NOT NULL,
    name                  TEXT NOT NULL,
    name_with_owner       TEXT NOT NULL,
    url                   TEXT NOT NULL,
    is_disabled           INTEGER NOT NULL DEFAULT 0,
    is_archived           INTEGER NOT NULL DEFAULT 0,
    pushed_at             TEXT,
    default_branch        TEXT,
    default_branch_status TEXT,
    owner_login           TEXT,
    owner_avatar          TEXT,
    owner_url             TEXT,
    PRIMARY KEY (actor, owner, name)
);

-- pull_requests rows come from three writers: the GraphQL org-repos fetch
-- (SetRepoPRs, which selects only the identity-locked GraphQL field set),
-- pull_request/pull_request_review webhooks (ParsePRPayload, full REST-shaped
-- objects), and the cached REST /pulls routes (absorbed responses). The
-- REST-only columns below (node_id .. merge_commit_sha) are NULL on
-- GraphQL-sourced rows; a row is "rest-complete" (rebuildable as a trimmed
-- REST response) only when node_id and base_ref_oid are set -- see
-- ghdata.PRRestComplete.
CREATE TABLE pull_requests (
    actor                TEXT NOT NULL DEFAULT '',
    owner                TEXT NOT NULL,
    repo                 TEXT NOT NULL,
    number               INTEGER NOT NULL,
    title                TEXT NOT NULL,
    url                  TEXT NOT NULL,
    is_draft             INTEGER NOT NULL DEFAULT 0,
    state                TEXT NOT NULL,
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL,
    additions            INTEGER,
    deletions            INTEGER,
    mergeable            TEXT,
    author_login         TEXT,
    author_avatar        TEXT,
    author_url           TEXT,
    head_ref_name        TEXT,
    base_ref_name        TEXT,
    head_ref_oid         TEXT,
    review_request_count INTEGER,
    last_commit_status   TEXT,
    node_id              TEXT,   -- GraphQL node id (REST/webhook sources only)
    body                 TEXT,   -- PR description; NULL = GitHub null body (or GraphQL-sourced row)
    author_type          TEXT,   -- user.type: User | Bot | Organization
    base_ref_oid         TEXT,   -- base.sha
    head_repo_full_name  TEXT,   -- head.repo.full_name; NULL when the head repo is gone (deleted fork)
    auto_merge_method    TEXT,   -- native auto-merge method when armed (merge|squash|rebase); NULL = not armed
    merge_commit_sha     TEXT,   -- GitHub's test-merge sha; NULL until computed
    PRIMARY KEY (actor, owner, repo, number)
);

CREATE TABLE pr_labels (
    actor       TEXT NOT NULL DEFAULT '',
    owner       TEXT NOT NULL,
    repo        TEXT NOT NULL,
    pr_number   INTEGER NOT NULL,
    name        TEXT NOT NULL,
    color       TEXT NOT NULL,
    PRIMARY KEY (actor, owner, repo, pr_number, name)
);

CREATE TABLE pr_files (
    actor       TEXT NOT NULL DEFAULT '',
    owner       TEXT NOT NULL,
    repo        TEXT NOT NULL,
    pr_number   INTEGER NOT NULL,
    path        TEXT NOT NULL,
    additions   INTEGER NOT NULL,
    deletions   INTEGER NOT NULL,
    PRIMARY KEY (actor, owner, repo, pr_number, path)
);

CREATE TABLE branch_comparisons (
    actor       TEXT NOT NULL DEFAULT '',
    owner       TEXT NOT NULL,
    repo        TEXT NOT NULL,
    base_ref    TEXT NOT NULL,
    head_ref    TEXT NOT NULL,
    ahead_by    INTEGER NOT NULL,
    behind_by   INTEGER NOT NULL,
    PRIMARY KEY (actor, owner, repo, base_ref, head_ref)
);

-- Per-check state for a commit, fed by status/check_run/check_suite webhooks.
-- We aggregate these into the PR's last_commit_status rollup without re-fetching
-- from GitHub. context is the status context or check name (latest state wins).
CREATE TABLE commit_checks (
    actor       TEXT NOT NULL DEFAULT '',
    owner       TEXT NOT NULL,
    repo        TEXT NOT NULL,
    sha         TEXT NOT NULL,
    context     TEXT NOT NULL,
    state       TEXT NOT NULL,   -- normalized: SUCCESS / FAILURE / ERROR / PENDING / EXPECTED
    PRIMARY KEY (actor, owner, repo, sha, context)
);

-- ============================================================================
-- Cached-route response state (trimmed rebuilds; see internal/api/respcache.go)
-- ============================================================================
--
-- These tables back the cached REST routes. They store the STATE contained in a
-- GitHub response (never the raw response bytes); the API layer rebuilds a
-- trimmed response body from this state, dropping every URL field (url, *_url,
-- _links). Rows are actor-partitioned like every other data table; invalidation
-- (webhook-driven) is global across actors, like MarkStaleByKindKey.

-- State for GET /repos/{owner}/{repo}/contents/{path}?ref=... responses.
-- owner/repo are stored lowercased (GitHub treats them case-insensitively in
-- URLs, and webhook invalidation must match regardless of the caller's casing);
-- path and ref are exact. kind is 'file' (name/sha/size/encoding/content set),
-- 'dir' (entries = JSON array of trimmed {type,size,name,path,sha} objects), or
-- 'missing' (a cached 404; message = GitHub's error message).
CREATE TABLE contents_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    actor        TEXT NOT NULL,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    path         TEXT NOT NULL,              -- exact file path ('' never cached; route requires one)
    ref          TEXT NOT NULL DEFAULT '',   -- ?ref= query value ('' = default branch)
    kind         TEXT NOT NULL,              -- file | dir | missing
    name         TEXT NOT NULL DEFAULT '',
    sha          TEXT NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    encoding     TEXT NOT NULL DEFAULT '',
    content      TEXT NOT NULL DEFAULT '',   -- base64 content exactly as GitHub sent it
    entries      TEXT NOT NULL DEFAULT '',   -- dir listings: JSON array of trimmed entries
    message      TEXT NOT NULL DEFAULT '',   -- missing: GitHub's 404 message
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks invalidate sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_contents_cache_key ON contents_cache (actor, owner, repo, path, ref);
CREATE INDEX idx_contents_cache_repo ON contents_cache (owner, repo);
CREATE INDEX idx_contents_cache_lru ON contents_cache (last_used_at);

-- State for GET /repos/{owner}/{repo}/git/commits/{sha} responses. A git commit
-- is immutable, so rows have no TTL and no webhook invalidation -- only LRU
-- pruning bounds them. Rows are written both by the API layer (on a fetch) and
-- by the webhook dispatcher (absorbed from push payload commits), and both must
-- rebuild to the same trimmed shape. parents is a comma-joined parent-sha list.
CREATE TABLE git_commits_cache (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    actor           TEXT NOT NULL,
    owner           TEXT NOT NULL,           -- lowercased
    repo            TEXT NOT NULL,           -- lowercased
    sha             TEXT NOT NULL,           -- lowercased full hex
    message         TEXT NOT NULL DEFAULT '',
    author_name     TEXT NOT NULL DEFAULT '',
    author_email    TEXT NOT NULL DEFAULT '',
    author_date     TEXT NOT NULL DEFAULT '',
    committer_name  TEXT NOT NULL DEFAULT '',
    committer_email TEXT NOT NULL DEFAULT '',
    committer_date  TEXT NOT NULL DEFAULT '',
    tree_sha        TEXT NOT NULL DEFAULT '',
    parents         TEXT NOT NULL DEFAULT '', -- comma-joined parent shas ('' = none)
    fetched_at      TEXT NOT NULL,            -- RFC3339
    last_used_at    TEXT NOT NULL             -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_git_commits_cache_key ON git_commits_cache (actor, owner, repo, sha);
CREATE INDEX idx_git_commits_cache_lru ON git_commits_cache (last_used_at);

-- State for POST /app/installations/{id}/access_tokens responses (the
-- installation-token mint cache). actor is the verified app identity
-- ("app:<id>"); body_hash is the SHA-256 of the canonicalized request body
-- (empty body vs permissions/repositories subsets mint DIFFERENT tokens).
-- token is a live short-lived credential at rest -- same trust domain as the
-- traffic itself, bounded by expiry (see the security notes in CLAUDE.md).
-- expires_at is the serve-until time: GitHub's token expiry minus a safety
-- buffer; past it the row is a miss and a fresh mint replaces it.
CREATE TABLE install_token_cache (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    actor                TEXT NOT NULL,             -- "app:<verified app id>"
    installation_id      TEXT NOT NULL,             -- from the URL path
    body_hash            TEXT NOT NULL,             -- SHA-256 of canonicalized request body
    token                TEXT NOT NULL,             -- minted installation token (secret)
    token_expires_at     TEXT NOT NULL,             -- GitHub's expires_at, verbatim
    permissions          TEXT NOT NULL DEFAULT '',  -- JSON object, '' when GitHub omitted it
    repository_selection TEXT NOT NULL DEFAULT '',
    fetched_at           TEXT NOT NULL,             -- RFC3339
    expires_at           TEXT NOT NULL,             -- RFC3339 serve-until (token expiry - buffer)
    last_used_at         TEXT NOT NULL              -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_install_token_cache_key ON install_token_cache (actor, installation_id, body_hash);
CREATE INDEX idx_install_token_cache_install ON install_token_cache (installation_id);
CREATE INDEX idx_install_token_cache_lru ON install_token_cache (last_used_at);

-- "Open-PR list complete" markers for GET /repos/{owner}/{repo}/pulls (the
-- cached open-PR list). A valid row means: for this actor, the pull_requests
-- table holds the repo's COMPLETE open-PR set (absorbed from a full REST
-- list response), so the route may rebuild the list from state. Webhook
-- pull_request events do NOT touch the marker -- they ARE the maintenance
-- (rows stay current); expires_at is only the TTL backstop bounding missed
-- deliveries. The GraphQL org-repos fetch (SetRepoPRs) REPLACES a repo's PR
-- rows with GraphQL-sourced rows that lack the REST-only columns, so it
-- deletes the marker (DeletePullsListMarker inside its transaction). owner
-- and repo are stored lowercased, like the other cached-route tables.
CREATE TABLE pulls_list_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    actor        TEXT NOT NULL,
    owner        TEXT NOT NULL,   -- lowercased
    repo         TEXT NOT NULL,   -- lowercased
    fetched_at   TEXT NOT NULL,   -- RFC3339
    expires_at   TEXT NOT NULL,   -- RFC3339 TTL backstop (webhooks maintain rows, never the marker)
    last_used_at TEXT NOT NULL    -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_pulls_list_cache_key ON pulls_list_cache (actor, owner, repo);
CREATE INDEX idx_pulls_list_cache_repo ON pulls_list_cache (owner, repo);
CREATE INDEX idx_pulls_list_cache_lru ON pulls_list_cache (last_used_at);

-- State for GET /repos/{owner}/{repo}/installation responses (an App-JWT-authed
-- endpoint, like the token mint: actor is the verified "app:<id>"). Invalidated
-- by installation/installation_repositories events for the stored installation
-- id, plus the TTL backstop. owner/repo lowercased.
CREATE TABLE repo_installation_cache (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    actor                TEXT NOT NULL,             -- "app:<verified app id>"
    owner                TEXT NOT NULL,             -- lowercased
    repo                 TEXT NOT NULL,             -- lowercased
    installation_id      INTEGER NOT NULL,
    account_login        TEXT NOT NULL DEFAULT '',
    account_type         TEXT NOT NULL DEFAULT '',  -- Organization | User
    repository_selection TEXT NOT NULL DEFAULT '',  -- all | selected
    app_id               INTEGER NOT NULL DEFAULT 0,
    app_slug             TEXT NOT NULL DEFAULT '',
    target_type          TEXT NOT NULL DEFAULT '',
    fetched_at           TEXT NOT NULL,             -- RFC3339
    expires_at           TEXT NOT NULL,             -- RFC3339 TTL backstop
    last_used_at         TEXT NOT NULL              -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_repo_installation_cache_key ON repo_installation_cache (actor, owner, repo);
CREATE INDEX idx_repo_installation_cache_install ON repo_installation_cache (installation_id);
CREATE INDEX idx_repo_installation_cache_lru ON repo_installation_cache (last_used_at);

-- ============================================================================
-- Actor Identities (dashboard only)
-- ============================================================================
--
-- Maps a cache partition (actor = SHA-256 fingerprint of a token) to the GitHub
-- login that token authenticated as. Populated in requireAuth whenever a token
-- is validated. This does NOT relax data isolation: the data tables remain keyed
-- by the opaque fingerprint. It exists purely so the web dashboard can group a
-- user's own scopes (a user may hold several tokens) under their login, and so
-- an admin can attribute every scope. The raw token is never stored — only its
-- fingerprint and the login GitHub reports for it.
CREATE TABLE actor_identities (
    actor       TEXT NOT NULL PRIMARY KEY,  -- token fingerprint (matches the actor column elsewhere)
    login       TEXT NOT NULL,              -- GitHub login the token authenticated as
    first_seen  TEXT NOT NULL,              -- RFC3339
    last_seen   TEXT NOT NULL               -- RFC3339
);

CREATE INDEX idx_actor_identities_login ON actor_identities (login);

-- ============================================================================
-- Webhook delivery log (dashboard observability)
-- ============================================================================

-- Every received webhook delivery and what the dispatcher did with it. Unlike
-- the per-credential data tables, this log is global (not actor-scoped): a
-- delivery is a single GitHub event, recorded once with its disposition so an
-- operator can see whether the cache was actually updated. delivery_id is the
-- X-GitHub-Delivery UUID, which matches the row in GitHub's "Recent Deliveries"
-- UI, so the two can be lined up. The log is capped to the most recent rows
-- (see PruneWebhookDeliveries) since it is observability, not source-of-truth.
CREATE TABLE webhook_deliveries (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    delivery_id  TEXT NOT NULL DEFAULT '',   -- X-GitHub-Delivery header (UUID)
    event_type   TEXT NOT NULL,              -- X-GitHub-Event header
    action       TEXT NOT NULL DEFAULT '',   -- payload "action", when present
    repo         TEXT NOT NULL DEFAULT '',   -- owner/name, when derivable
    received_at  TEXT NOT NULL,              -- RFC3339
    disposition  TEXT NOT NULL,              -- applied | skipped | invalidated | ignored | error
    detail       TEXT NOT NULL DEFAULT '',   -- human summary, e.g. "upserted PR #42"
    actors       INTEGER NOT NULL DEFAULT 0  -- number of cache scopes touched
);

-- ============================================================================
-- Workflow jobs (webhook-fed Actions job state)
-- ============================================================================

-- GitHub Actions job state, fed by workflow_job webhooks (in_progress and
-- completed actions; queued/waiting churn is deliberately not recorded). Like
-- webhook_deliveries, this table is GLOBAL (not actor-scoped): it is webhook-fed
-- operational telemetry with no per-credential fetch path — a job's state only
-- ever arrives via the HMAC-verified delivery, never through a caller's token,
-- so there is no credential to partition by (and the read path is admin-only).
-- Empty string means "not reported" for the optional TEXT fields, matching the
-- webhook_deliveries convention. Rows are bounded by pruning on write: completed
-- jobs older than a retention window are deleted after each upsert (see
-- PruneWorkflowJobs and ghdata.workflowJobRetention).
CREATE TABLE workflow_jobs (
    owner         TEXT NOT NULL,
    repo          TEXT NOT NULL,
    job_id        INTEGER NOT NULL,
    run_id        INTEGER NOT NULL DEFAULT 0,
    run_attempt   INTEGER NOT NULL DEFAULT 0,
    name          TEXT NOT NULL DEFAULT '',   -- job name
    workflow_name TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL,              -- in_progress | completed
    conclusion    TEXT NOT NULL DEFAULT '',   -- success | failure | ... (completed only)
    head_sha      TEXT NOT NULL DEFAULT '',
    head_branch   TEXT NOT NULL DEFAULT '',
    html_url      TEXT NOT NULL DEFAULT '',
    started_at    TEXT NOT NULL DEFAULT '',   -- RFC3339
    completed_at  TEXT NOT NULL DEFAULT '',   -- RFC3339
    runner_name   TEXT NOT NULL DEFAULT '',   -- null in the payload until assigned
    updated_at    TEXT NOT NULL,              -- RFC3339: when the last webhook was applied
    PRIMARY KEY (owner, repo, job_id)
);

-- Makes the on-write prune (DELETE ... WHERE status='completed' AND
-- completed_at < cutoff) a single indexed scan of only the completed rows.
CREATE INDEX idx_workflow_jobs_completed_at
    ON workflow_jobs (completed_at) WHERE status = 'completed';
