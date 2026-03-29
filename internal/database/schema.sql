-- Schema version: bump this constant in db.go when changing this file.
-- The DB is a cache — on version mismatch, the file gets deleted and recreated.

-- ============================================================================
-- Freshness / Cache Metadata (generic, no GitHub knowledge)
-- ============================================================================

CREATE TABLE schema_version (
    version INTEGER NOT NULL
);

CREATE TABLE cache_metadata (
    resource_kind   TEXT NOT NULL,
    resource_key    TEXT NOT NULL,
    last_fetched_at TEXT,           -- RFC3339
    last_changed_at TEXT,           -- RFC3339
    etag            TEXT,
    expires_at      TEXT,           -- RFC3339
    fetch_state     TEXT NOT NULL DEFAULT 'unknown',
    error_message   TEXT,
    retry_after     TEXT,           -- RFC3339
    PRIMARY KEY (resource_kind, resource_key)
);

CREATE TABLE cache_refresh_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
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
    login       TEXT PRIMARY KEY,
    avatar_url  TEXT NOT NULL,
    url         TEXT NOT NULL
);

CREATE TABLE orgs (
    login       TEXT PRIMARY KEY,
    avatar_url  TEXT,
    url         TEXT
);

CREATE TABLE user_org_memberships (
    user_login  TEXT NOT NULL,
    org_login   TEXT NOT NULL,
    PRIMARY KEY (user_login, org_login)
);

CREATE TABLE repos (
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
    PRIMARY KEY (owner, name)
);

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

CREATE TABLE pr_files (
    owner       TEXT NOT NULL,
    repo        TEXT NOT NULL,
    pr_number   INTEGER NOT NULL,
    path        TEXT NOT NULL,
    additions   INTEGER NOT NULL,
    deletions   INTEGER NOT NULL,
    PRIMARY KEY (owner, repo, pr_number, path)
);

CREATE TABLE branch_comparisons (
    owner       TEXT NOT NULL,
    repo        TEXT NOT NULL,
    base_ref    TEXT NOT NULL,
    head_ref    TEXT NOT NULL,
    ahead_by    INTEGER NOT NULL,
    behind_by   INTEGER NOT NULL,
    PRIMARY KEY (owner, repo, base_ref, head_ref)
);
