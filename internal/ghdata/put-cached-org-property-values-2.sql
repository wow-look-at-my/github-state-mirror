
DELETE FROM org_property_values_cache WHERE rowid IN (
    SELECT rowid FROM org_property_values_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
)