
INSERT INTO workflow_run_details (owner, repo, run_id, updated_at, detail, detail_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, run_id) DO UPDATE SET
    updated_at = excluded.updated_at,
    detail = excluded.detail,
    detail_at = excluded.detail_at
WHERE excluded.updated_at >= workflow_run_details.updated_at