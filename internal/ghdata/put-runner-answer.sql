
INSERT INTO runner_answer_cache (token_fp, scope, runner_id, per_page, page, status, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (token_fp, scope, runner_id, per_page, page) DO UPDATE SET
    status = excluded.status,
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at