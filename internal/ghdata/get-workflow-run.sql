
SELECT w.owner, w.repo, w.run_id, w.run_attempt, w.name, w.head_sha, w.head_branch,
       w.status, w.conclusion, w.html_url, w.created_at, w.updated_at, w.run_started_at,
       d.detail, d.detail_at
FROM workflow_runs w
LEFT JOIN workflow_run_details d ON d.owner = w.owner AND d.repo = w.repo AND d.run_id = w.run_id
WHERE w.owner = ? AND w.repo = ? AND w.run_id = ?