
DELETE FROM repo_answer_cache WHERE id IN (
    SELECT id FROM repo_answer_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
)