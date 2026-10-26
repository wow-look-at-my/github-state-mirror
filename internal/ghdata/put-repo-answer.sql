
INSERT INTO repo_answer_cache (owner, repo, kind, ref_id, shape, subject, status, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (owner, repo, kind, ref_id, shape) DO UPDATE SET
    subject = excluded.subject,
    status = excluded.status,
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at