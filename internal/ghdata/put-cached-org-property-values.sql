
INSERT INTO org_property_values_cache (token_fp, org, per_page, page, status, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (token_fp, org, per_page, page) DO UPDATE SET
    status = excluded.status,
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at