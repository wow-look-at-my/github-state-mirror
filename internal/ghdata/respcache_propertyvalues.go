package ghdata

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// State for GET /orgs/{org}/properties/values; see docs/cache/rest-routes.md.

const (
	// OrgPropertyValuesCacheTTL backstops a lost custom_property_values delivery.
	OrgPropertyValuesCacheTTL = 10 * time.Minute

	// A permission grant can change with no delivery.
	OrgPropertyValuesVerdictTTL = 5 * time.Minute
)

// OrgPropertyValuesEntry is one repo of the trimmed answer.
type OrgPropertyValuesEntry struct {
	RepositoryID       int64              `json:"repository_id"`
	RepositoryName     string             `json:"repository_name"`
	RepositoryFullName string             `json:"repository_full_name"`
	Properties         []OrgPropertyValue `json:"properties"`
}

// OrgPropertyValue is one property of a repo. Value is a string, an array of
// strings, or null, so it is kept as raw JSON.
type OrgPropertyValue struct {
	PropertyName string          `json:"property_name"`
	Value        json.RawMessage `json:"value"`
}

// OrgPropertyValuesAnswer is one stored answer.
type OrgPropertyValuesAnswer struct {
	Status int
	Doc    string
}

const orgPropertyValuesKeyWhere = `token_fp = ? AND org = ? AND per_page = ? AND page = ?`

// GetCachedOrgPropertyValues returns the stored answer, or false on a miss or
// an expired row. A hit refreshes the LRU stamp.
func (s *Store) GetCachedOrgPropertyValues(ctx context.Context, tokenFP, org string, perPage, page int64, now time.Time) (OrgPropertyValuesAnswer, bool, error) {
	args := []any{tokenFP, NormalizeRepoKey(org), perPage, page}
	var a OrgPropertyValuesAnswer
	var expires string
	err := s.dbx(ctx).QueryRowContext(ctx,
		`SELECT status, doc, expires_at FROM org_property_values_cache WHERE `+orgPropertyValuesKeyWhere, args...,
	).Scan(&a.Status, &a.Doc, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return OrgPropertyValuesAnswer{}, false, nil
	}
	if err != nil {
		return OrgPropertyValuesAnswer{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339, expires); perr != nil || !exp.After(now) {
		return OrgPropertyValuesAnswer{}, false, nil
	}
	_, _ = s.dbx(ctx).ExecContext(ctx,
		`UPDATE org_property_values_cache SET last_used_at = ? WHERE `+orgPropertyValuesKeyWhere,
		append([]any{rfc3339(now)}, args...)...)
	return a, true, nil
}

// PutCachedOrgPropertyValues stores an answer, then prunes expired rows and
// the LRU tail beyond the cap.
func (s *Store) PutCachedOrgPropertyValues(ctx context.Context, tokenFP, org string, perPage, page int64, a OrgPropertyValuesAnswer, now time.Time, ttl time.Duration) error {
	db := s.dbx(ctx)
	if _, err := db.ExecContext(ctx, `
INSERT INTO org_property_values_cache (token_fp, org, per_page, page, status, doc, fetched_at, expires_at, last_used_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (token_fp, org, per_page, page) DO UPDATE SET
    status = excluded.status,
    doc = excluded.doc,
    fetched_at = excluded.fetched_at,
    expires_at = excluded.expires_at,
    last_used_at = excluded.last_used_at`,
		tokenFP, NormalizeRepoKey(org), perPage, page, a.Status, a.Doc, rfc3339(now), rfc3339(now.Add(ttl)), rfc3339(now),
	); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM org_property_values_cache WHERE expires_at <= ?`, rfc3339(now)); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
DELETE FROM org_property_values_cache WHERE rowid IN (
    SELECT rowid FROM org_property_values_cache ORDER BY last_used_at DESC LIMIT -1 OFFSET ?
)`, CacheMaxRows)
	return err
}

// InvalidateOrgPropertyValues drops an org's answers across every credential.
func (s *Store) InvalidateOrgPropertyValues(ctx context.Context, org string) error {
	_, err := s.dbx(ctx).ExecContext(ctx, `DELETE FROM org_property_values_cache WHERE org = ?`, NormalizeRepoKey(org))
	return err
}

// ApplyOrgPropertyValues writes a repo's changed property values into every
// stored page that lists the repo. It reports false when a page cannot take
// the change in place: a property the entry does not list, or a value that
// became null. The caller then flushes the org.
func (s *Store) ApplyOrgPropertyValues(ctx context.Context, org string, repoID int64, values []OrgPropertyValue, now time.Time) (bool, error) {
	db := s.dbx(ctx)
	rows, err := db.QueryContext(ctx, `
SELECT token_fp, per_page, page, doc FROM org_property_values_cache
WHERE org = ? AND status = 200 AND expires_at > ?`, NormalizeRepoKey(org), rfc3339(now))
	if err != nil {
		return false, err
	}
	type update struct {
		fp            string
		perPage, page int64
		doc           string
	}
	var updates []update
	for rows.Next() {
		var u update
		var doc string
		if err := rows.Scan(&u.fp, &u.perPage, &u.page, &doc); err != nil {
			rows.Close()
			return false, err
		}
		var entries []OrgPropertyValuesEntry
		if err := json.Unmarshal([]byte(doc), &entries); err != nil {
			rows.Close()
			return false, nil
		}
		changed := false
		for i := range entries {
			if entries[i].RepositoryID != repoID {
				continue
			}
			if !applyPropertyValues(&entries[i], values) {
				rows.Close()
				return false, nil
			}
			changed = true
		}
		if !changed {
			continue
		}
		rendered, err := MarshalCacheDoc(entries)
		if err != nil {
			rows.Close()
			return false, err
		}
		u.doc = string(rendered)
		updates = append(updates, u)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, u := range updates {
		if _, err := db.ExecContext(ctx,
			`UPDATE org_property_values_cache SET doc = ? WHERE `+orgPropertyValuesKeyWhere,
			u.doc, u.fp, NormalizeRepoKey(org), u.perPage, u.page); err != nil {
			return false, err
		}
	}
	return true, nil
}

// applyPropertyValues replaces each named value in place. It reports false
// for a change the entry cannot take as it stands.
func applyPropertyValues(e *OrgPropertyValuesEntry, values []OrgPropertyValue) bool {
	for _, v := range values {
		if len(bytes.TrimSpace(v.Value)) == 0 || bytes.Equal(bytes.TrimSpace(v.Value), []byte("null")) {
			return false
		}
		found := false
		for i := range e.Properties {
			if e.Properties[i].PropertyName == v.PropertyName {
				e.Properties[i].Value = v.Value
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
