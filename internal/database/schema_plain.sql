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
