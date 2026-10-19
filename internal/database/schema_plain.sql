-- Tables whose queries are plain SQL in Go (internal/ghdata), outside sqlc.
-- A branch push may not change a generated file, so a new table lands here.
-- In every other way this file is part of the schema: db.go creates it with
-- schema.sql and hashes both into the fingerprint, so editing a table here
-- nukes the cache DB too.

-- The rest of the single-run read, GET /repos/{owner}/{repo}/actions/runs/{id}:
-- the run fields the workflow_runs truth row does not hold, as JSON. Only a
-- source that carries the whole run object writes a row: a workflow_run
-- delivery or that read's own fetch. updated_at is the run's own, so an older
-- view never overwrites a newer one. detail_at is when a whole object last
-- landed, which bounds how long the read serves without a fresh one.
CREATE TABLE workflow_run_details (
    owner      TEXT NOT NULL,              -- lowercased
    repo       TEXT NOT NULL,              -- lowercased
    run_id     INTEGER NOT NULL,
    updated_at TEXT NOT NULL,              -- RFC3339, GitHub's own
    detail     TEXT NOT NULL,              -- JSON, ghdata.WorkflowRunDetail
    detail_at  TEXT NOT NULL,              -- RFC3339
    PRIMARY KEY (owner, repo, run_id)
);

CREATE INDEX idx_workflow_run_details_at ON workflow_run_details (detail_at);

-- Whole-document answers for the repo-scoped routes that share one row space.
-- kind names the route. ref_id is the resource inside the repo, VERBATIM as
-- requested; shape is the canonical modeled query. subject is what a delivery
-- names to flush the row (a check run id, a branch). status is the answer's
-- HTTP status: a 200 document, a 302 whose doc holds the Location, or a
-- stored verdict. expires_at is per row, because a signed redirect expires on
-- its own clock.
CREATE TABLE repo_answer_cache (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner        TEXT NOT NULL,              -- lowercased
    repo         TEXT NOT NULL,              -- lowercased
    kind         TEXT NOT NULL,              -- ghdata.RepoAnswerKind*
    ref_id       TEXT NOT NULL DEFAULT '',   -- the resource inside the repo, verbatim
    shape        TEXT NOT NULL DEFAULT '',   -- canonical modeled query
    subject      TEXT NOT NULL DEFAULT '',   -- what a delivery names to flush this row
    status       INTEGER NOT NULL,
    doc          TEXT NOT NULL,              -- rendered trimmed document
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339
    last_used_at TEXT NOT NULL               -- RFC3339, for LRU pruning
);

CREATE UNIQUE INDEX idx_repo_answer_cache_key ON repo_answer_cache (owner, repo, kind, ref_id, shape);
CREATE INDEX idx_repo_answer_cache_subject ON repo_answer_cache (owner, repo, kind, subject);
CREATE INDEX idx_repo_answer_cache_lru ON repo_answer_cache (last_used_at);

-- Answers of GET /orgs/{org}/properties/values, one row per credential and
-- page. The answer lists the org repos THIS token can see, so a row is only
-- replayed to the credential GitHub answered. status is 200, or a stored
-- 403/404 verdict. doc is the rendered trimmed document.
CREATE TABLE org_property_values_cache (
    token_fp     TEXT NOT NULL,
    org          TEXT NOT NULL,              -- lowercased
    per_page     INTEGER NOT NULL,
    page         INTEGER NOT NULL,
    status       INTEGER NOT NULL,
    doc          TEXT NOT NULL,
    fetched_at   TEXT NOT NULL,              -- RFC3339
    expires_at   TEXT NOT NULL,              -- RFC3339
    last_used_at TEXT NOT NULL,              -- RFC3339, for LRU pruning
    PRIMARY KEY (token_fp, org, per_page, page)
);

CREATE INDEX idx_org_property_values_cache_org ON org_property_values_cache (org);
CREATE INDEX idx_org_property_values_cache_lru ON org_property_values_cache (last_used_at);
