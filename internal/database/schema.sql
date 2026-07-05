-- Schema version: bump this constant in db.go when changing this file.
-- The DB is a cache -- on version mismatch, the file gets deleted and recreated.
--
-- DATA MODEL (since v9): ONE GLOBAL TRUTH STORE. GitHub state tables (repos,
-- pull_requests, pr_labels, commit_checks, contents_cache, git_commits_cache)
-- hold ONE row per resource -- no actor/scope dimension. Webhooks and fetches
-- by any principal all write the same truth. What a caller may READ is decided
-- at serve time by the reveal-by-permission layer: a repo's cached state is
-- revealed to a principal iff the repo is public in global truth, or the
-- principal holds a fresh access_grants row for it (earned from GitHub's own
-- answers to that principal's requests). See CLAUDE.md "security model".

-- ============================================================================
-- Freshness / Cache Metadata (generic, no GitHub knowledge)
-- ============================================================================

CREATE TABLE schema_version (
    version INTEGER NOT NULL
);

-- Freshness markers. Two kinds of rows share this table:
--   - actor = a principal ("user:<id>", "app:<id>", "app-installation:<id>",
--     or a token fingerprint): that principal's per-owner org-repos LIST-SYNC
--     marker (kind 'org_repos'). Freshness of the principal's GRANT SET for
--     the owner, not of the data itself -- truth is refreshed as a side effect
--     of any principal's sync.
--   - actor = 'global': freshness of a piece of GLOBAL truth that has a
--     dedicated fetch path (kind 'repo_pulls', key 'owner/repo'). Any
--     principal's fetch refreshes it for everyone.
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
-- GitHub Data Tables (GLOBAL truth -- one row per resource)
-- ============================================================================

-- visibility is the reveal layer's fast path: 'public' rows are readable by any
-- authenticated principal without a grant. It is learned from webhook payloads
-- (repository.private / repository.visibility, flipped by publicized/privatized
-- events) and from REST fetch payloads that carry it; the identity-locked
-- GraphQL org-repos selection set does NOT carry it, so a repo seeded only by
-- that fetch stays '' (unknown) and is treated as PRIVATE (fail closed) until a
-- webhook or REST absorb reveals it.
CREATE TABLE repos (
    owner                 TEXT NOT NULL,
    name                  TEXT NOT NULL,
    name_with_owner       TEXT NOT NULL,
    url                   TEXT NOT NULL,
    is_disabled           INTEGER NOT NULL DEFAULT 0,
    is_archived           INTEGER NOT NULL DEFAULT 0,
    visibility            TEXT NOT NULL DEFAULT '',  -- '' unknown | public | private | internal
    pushed_at             TEXT,
    default_branch        TEXT,
    default_branch_status TEXT,
    owner_login           TEXT,
    owner_avatar          TEXT,
    owner_url             TEXT,
    PRIMARY KEY (owner, name)
);

-- pull_requests rows come from three writers: the GraphQL org-repos fetch
-- (the org sync, which selects only the identity-locked GraphQL field set),
-- pull_request/pull_request_review webhooks (ParsePRPayload, full REST-shaped
-- objects), and the cached REST /pulls routes (absorbed responses). The
-- REST-only columns (node_id .. merge_commit_sha) are NULL on GraphQL-sourced
-- rows; a row is "rest-complete" (rebuildable as a trimmed REST response)
-- only when node_id and base_ref_oid are set -- see ghdata.PRRestComplete.
--
-- touched_at guards reconciles against racing webhooks (an org/pulls fetch
-- only deletes an open-PR row absent from its snapshot when the row was not
-- touched after the fetch began; GraphQL/REST list reads are eventually
-- consistent, so a just-webhooked PR can be missing from a snapshot taken
-- moments later) AND backstops missed close deliveries: a row untouched for
-- longer than the staleness window is not served by the single-PR route.
CREATE TABLE pull_requests (
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
    touched_at           TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (owner, repo, number)
);

CREATE TABLE pr_labels (
    owner       TEXT NOT NULL,
    repo        TEXT NOT NULL,
    pr_number   INTEGER NOT NULL,
    name        TEXT NOT NULL,
    color       TEXT NOT NULL,
    PRIMARY KEY (owner, repo, pr_number, name)
);

-- Per-check state for a commit, fed by status/check_run/check_suite webhooks.
-- We aggregate these into the PR's last_commit_status rollup without re-fetching
-- from GitHub. context is the status context or check name (latest state wins).
CREATE TABLE commit_checks (
    owner       TEXT NOT NULL,
    repo        TEXT NOT NULL,
    sha         TEXT NOT NULL,
    context     TEXT NOT NULL,
    state       TEXT NOT NULL,   -- normalized: SUCCESS / FAILURE / ERROR / PENDING / EXPECTED
    PRIMARY KEY (owner, repo, sha, context)
);

-- ============================================================================
-- Reveal-by-permission: grants and deny verdicts
-- ============================================================================

-- A grant records that GitHub itself proved a principal can read a repo:
--   - source 'list_sync': the repo appeared in an org-repos fetch made with
--     that principal's own token (replace-synced per principal+owner on every
--     sync; absence from a sync revokes list_sync grants).
--   - source 'probe': a repo-scoped cached-route fetch with the principal's
--     token answered 2xx.
-- Grants expire (expires_at = granted_at + the grant TTL) so revoked access
-- ages out; an authoritative 403 on a later fetch deletes the grant
-- immediately. A plain 404 does NOT revoke (it is ambiguous with
-- "resource missing inside an accessible repo").
CREATE TABLE access_grants (
    principal  TEXT NOT NULL,  -- "user:<id>", "app:<id>", "app-installation:<id>", or token fingerprint
    owner      TEXT NOT NULL,  -- lowercased
    repo       TEXT NOT NULL,  -- lowercased
    granted_at TEXT NOT NULL,  -- RFC3339
    expires_at TEXT NOT NULL,  -- RFC3339
    source     TEXT NOT NULL,  -- list_sync | probe
    PRIMARY KEY (principal, owner, repo)
);

CREATE INDEX idx_access_grants_repo ON access_grants (owner, repo);

-- A deny verdict caches GitHub's own authoritative "no" (404, or a
-- non-rate-limit 403) to ONE principal's probe of ONE resource, so an
-- unauthorized caller repeating the same request does not hammer GitHub.
-- Short TTL (~5m); only authoritative answers are stored -- transient
-- failures (5xx, 429, rate-limited 403) are never cached as denials. Keyed by
-- the exact resource (not the whole repo) because GitHub's 404 cannot be told
-- apart from "file/PR missing inside a repo the principal CAN see". Earning a
-- grant for the repo clears the principal's verdicts for it.
CREATE TABLE deny_cache (
    principal     TEXT NOT NULL,
    resource_kind TEXT NOT NULL,  -- contents | git_commit | repo_pulls | pull | repo_commits
    resource_key  TEXT NOT NULL,  -- route-specific resource key
    owner         TEXT NOT NULL,  -- lowercased
    repo          TEXT NOT NULL,  -- lowercased
    status        INTEGER NOT NULL,  -- 404 or 403
    message       TEXT NOT NULL DEFAULT '',
    denied_at     TEXT NOT NULL,  -- RFC3339
    expires_at    TEXT NOT NULL,  -- RFC3339
    PRIMARY KEY (principal, resource_kind, resource_key)
);

CREATE INDEX idx_deny_cache_repo ON deny_cache (principal, owner, repo);

-- ============================================================================
-- Cached-route response state (trimmed rebuilds; see internal/api/respcache.go)
-- ============================================================================
--
-- These tables back the cached REST routes. They store the STATE contained in a
-- GitHub response (never the raw response bytes); the API layer rebuilds a
-- trimmed response body from this state, dropping every URL field (url, *_url,
-- _links). Global truth like every other data table; who may read a row is the
-- reveal layer's job. Invalidation (webhook-driven) deletes rows outright.

-- State for GET /repos/{owner}/{repo}/contents/{path}?ref=... responses.
-- owner/repo are stored lowercased (GitHub treats them case-insensitively in
-- URLs, and webhook invalidation must match regardless of the caller's casing);
-- path and ref are exact. kind is 'file' (name/sha/size/encoding/content set),
-- 'dir' (entries = JSON array of trimmed {type,size,name,path,sha} objects), or
-- 'missing' (a cached 404; message = GitHub's error message). A 'missing' row
-- is only ever absorbed from a REVEALED principal's fetch (public repo or
-- grant held) -- an unauthorized probe's 404 is ambiguous and goes to
-- deny_cache instead, never into global truth.
CREATE TABLE contents_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
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

CREATE UNIQUE INDEX idx_contents_cache_key ON contents_cache (owner, repo, path, ref);
CREATE INDEX idx_contents_cache_lru ON contents_cache (last_used_at);

-- State for GET /repos/{owner}/{repo}/git/commits/{sha} responses. A git commit
-- is immutable, so rows have no TTL and no webhook invalidation -- only LRU
-- pruning bounds them. Rows are written both by the API layer (on a fetch) and
-- by the webhook dispatcher (absorbed from push payload commits), and both must
-- rebuild to the same trimmed shape. parents is a comma-joined parent-sha list.
CREATE TABLE git_commits_cache (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
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

CREATE UNIQUE INDEX idx_git_commits_cache_key ON git_commits_cache (owner, repo, sha);
CREATE INDEX idx_git_commits_cache_lru ON git_commits_cache (last_used_at);

-- Per-page snapshots for GET /repos/{owner}/{repo}/commits (the commits LIST
-- route). The listed commits themselves are absorbed into the git_commits_cache
-- rows above (the same global truth the single git-commit route and push
-- payloads maintain); a snapshot row stores only the ORDERING/COMPLETENESS
-- proof for one exact modeled query shape: the response's commit shas, in
-- order, keyed by (owner, repo, ref_param, per_page, page) where ref_param is
-- the raw ?sha= query value ('' = default branch). A hit requires an unexpired
-- snapshot AND every listed sha still resolving in git_commits_cache (an
-- LRU-pruned commit degrades the snapshot to a miss). Listings are
-- ref-tip-relative and move on every push, so push/repository webhooks flush a
-- repo's snapshots (the absorbed commit rows are immutable and stay);
-- expires_at is the 24h TTL backstop for missed deliveries. owner/repo
-- lowercased like the other cached-route tables.
CREATE TABLE commits_list_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    ref_param    TEXT NOT NULL DEFAULT '',   -- raw ?sha= query value ('' = default branch)
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    shas         TEXT NOT NULL,              -- JSON array of commit shas in response order
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339 TTL backstop (webhooks flush sooner)
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_commits_list_cache_key ON commits_list_cache (owner, repo, ref_param, per_page, page);
CREATE INDEX idx_commits_list_cache_lru ON commits_list_cache (last_used_at);

-- State for POST /app/installations/{id}/access_tokens responses (the
-- installation-token mint cache). This table stays keyed by the verified app
-- identity ("app:<id>") -- it caches a CREDENTIAL minted for that app, not
-- GitHub state, so it is deliberately outside the global-truth model.
-- body_hash is the SHA-256 of the canonicalized request body (empty body vs
-- permissions/repositories subsets mint DIFFERENT tokens). token is a live
-- short-lived credential at rest -- same trust domain as the traffic itself,
-- bounded by expiry (see the security notes in CLAUDE.md). expires_at is the
-- serve-until time: GitHub's token expiry minus a safety buffer; past it the
-- row is a miss and a fresh mint replaces it.
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
-- cached open-PR list). A valid row means: the GLOBAL pull_requests table
-- holds the repo's COMPLETE open-PR set (absorbed from a full REST list
-- response), so the route may rebuild the list from state. Webhook
-- pull_request events do NOT touch the marker -- they ARE the maintenance
-- (rows stay current); expires_at is only the TTL backstop bounding missed
-- deliveries. Who may READ the rebuilt list is the reveal layer's job. owner
-- and repo are stored lowercased, like the other cached-route tables.
CREATE TABLE pulls_list_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,   -- lowercased
    repo         TEXT NOT NULL,   -- lowercased
    fetched_at   TEXT NOT NULL,   -- RFC3339
    expires_at   TEXT NOT NULL,   -- RFC3339 TTL backstop (webhooks maintain rows, never the marker)
    last_used_at TEXT NOT NULL    -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_pulls_list_cache_key ON pulls_list_cache (owner, repo);
CREATE INDEX idx_pulls_list_cache_lru ON pulls_list_cache (last_used_at);

-- State for GET /repos/{owner}/{repo}/installation responses (an App-JWT-authed
-- endpoint, like the token mint: actor is the verified "app:<id>"). The answer
-- is app-specific -- each app has its own installations -- so this stays keyed
-- by app identity, deliberately outside the global-truth model. Invalidated by
-- installation/installation_repositories events for the stored installation
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
-- Principal Identities (dashboard only)
-- ============================================================================
--
-- Maps a principal (the reveal-layer identity: "user:<id>", "app:<id>", or a
-- token fingerprint) to the GitHub login it authenticated as. Populated in
-- requireAuth whenever a token is validated. Purely for the dashboard: it
-- groups a signed-in user's principal under their login and lets an admin
-- attribute every principal. The raw token is never stored -- only the
-- principal key and the login GitHub reports for it.
CREATE TABLE actor_identities (
    actor       TEXT NOT NULL PRIMARY KEY,  -- principal key (matches access_grants.principal)
    login       TEXT NOT NULL,              -- GitHub login the credential authenticated as
    first_seen  TEXT NOT NULL,              -- RFC3339
    last_seen   TEXT NOT NULL               -- RFC3339
);

CREATE INDEX idx_actor_identities_login ON actor_identities (login);

-- ============================================================================
-- Webhook delivery log (dashboard observability)
-- ============================================================================

-- Every received webhook delivery and what the dispatcher did with it. Global
-- (one GitHub event = one row). Since v9 every stateful event applies straight
-- to global truth, so the old 'skipped' disposition ("no cache scope had this
-- repo") no longer exists; 'ignored' remains for genuinely untracked event
-- types/actions. delivery_id is the X-GitHub-Delivery UUID, which matches the
-- row in GitHub's "Recent Deliveries" UI, so the two can be lined up. The log
-- is capped to the most recent rows (see PruneWebhookDeliveries) since it is
-- observability, not source-of-truth.
CREATE TABLE webhook_deliveries (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    delivery_id  TEXT NOT NULL DEFAULT '',   -- X-GitHub-Delivery header (UUID)
    event_type   TEXT NOT NULL,              -- X-GitHub-Event header
    action       TEXT NOT NULL DEFAULT '',   -- payload "action", when present
    repo         TEXT NOT NULL DEFAULT '',   -- owner/name, when derivable
    received_at  TEXT NOT NULL,              -- RFC3339
    disposition  TEXT NOT NULL,              -- applied | invalidated | ignored | error
    detail       TEXT NOT NULL DEFAULT ''    -- human summary, e.g. "upserted PR #42"
);

-- ============================================================================
-- Workflow jobs (webhook-fed Actions job state)
-- ============================================================================

-- GitHub Actions job state, fed by workflow_job webhooks (in_progress and
-- completed actions; queued/waiting churn is deliberately not recorded). Global
-- webhook-fed operational telemetry (the read path is admin-only). Empty string
-- means "not reported" for the optional TEXT fields, matching the
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
