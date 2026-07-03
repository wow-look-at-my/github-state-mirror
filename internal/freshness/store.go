package freshness

import (
	"context"
	"database/sql"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Store persists cache metadata to SQLite, wrapping the sqlc-generated queries.
type Store struct {
	q *dbgen.Queries
}

func NewStore(db *sql.DB) *Store {
	return &Store{q: dbgen.New(db)}
}

func (s *Store) Get(ctx context.Context, id ResourceID) (*Metadata, error) {
	row, err := s.q.GetCacheMetadata(ctx, dbgen.GetCacheMetadataParams{
		Actor:        id.Actor,
		ResourceKind: id.Kind,
		ResourceKey:  id.Key,
	})
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rowToMetadata(row), nil
}

func (s *Store) Upsert(ctx context.Context, m *Metadata) error {
	return s.q.UpsertCacheMetadata(ctx, dbgen.UpsertCacheMetadataParams{
		Actor:         m.Actor,
		ResourceKind:  m.Kind,
		ResourceKey:   m.Key,
		LastFetchedAt: optionalTime(m.LastFetchedAt),
		LastChangedAt: optionalTime(m.LastChangedAt),
		Etag:          nullString(m.ETag),
		ExpiresAt:     optionalTime(m.ExpiresAt),
		FetchState:    string(m.State),
		ErrorMessage:  nullString(m.ErrorMessage),
		RetryAfter:    optionalTime(m.RetryAfter),
	})
}

func (s *Store) MarkFetching(ctx context.Context, id ResourceID) error {
	return s.q.MarkFetching(ctx, dbgen.MarkFetchingParams{
		Actor:        id.Actor,
		ResourceKind: id.Kind,
		ResourceKey:  id.Key,
	})
}

func (s *Store) MarkFresh(ctx context.Context, id ResourceID, etag string, expiresAt time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return s.q.MarkFresh(ctx, dbgen.MarkFreshParams{
		LastFetchedAt: sql.NullString{String: now, Valid: true},
		Etag:          nullString(etag),
		ExpiresAt:     sql.NullString{String: expiresAt.UTC().Format(time.RFC3339), Valid: true},
		Actor:         id.Actor,
		ResourceKind:  id.Kind,
		ResourceKey:   id.Key,
	})
}

func (s *Store) MarkStale(ctx context.Context, id ResourceID) error {
	return s.q.MarkStale(ctx, dbgen.MarkStaleParams{
		Actor:        id.Actor,
		ResourceKind: id.Kind,
		ResourceKey:  id.Key,
	})
}

func (s *Store) MarkStaleAllActors(ctx context.Context, kind, key string) error {
	return s.q.MarkStaleByKindKey(ctx, dbgen.MarkStaleByKindKeyParams{
		ResourceKind: kind,
		ResourceKey:  key,
	})
}

func (s *Store) MarkError(ctx context.Context, id ResourceID, errMsg string, retryAfter time.Time) error {
	return s.q.MarkError(ctx, dbgen.MarkErrorParams{
		ErrorMessage: nullString(errMsg),
		RetryAfter:   sql.NullString{String: retryAfter.UTC().Format(time.RFC3339), Valid: true},
		Actor:        id.Actor,
		ResourceKind: id.Kind,
		ResourceKey:  id.Key,
	})
}

func (s *Store) ListByKind(ctx context.Context, actor, kind string) ([]Metadata, error) {
	rows, err := s.q.ListByKind(ctx, dbgen.ListByKindParams{
		Actor:        actor,
		ResourceKind: kind,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Metadata, len(rows))
	for i, r := range rows {
		out[i] = *rowToMetadata(r)
	}
	return out, nil
}

// ListByKindKeyAllActors returns every actor's metadata row for one
// (kind, key) resource -- e.g. all principals' org-sync markers for an owner.
func (s *Store) ListByKindKeyAllActors(ctx context.Context, kind, key string) ([]Metadata, error) {
	rows, err := s.q.ListByKindKey(ctx, dbgen.ListByKindKeyParams{
		ResourceKind: kind,
		ResourceKey:  key,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Metadata, len(rows))
	for i, r := range rows {
		out[i] = *rowToMetadata(r)
	}
	return out, nil
}

func (s *Store) ListStale(ctx context.Context, actor string, before time.Time) ([]Metadata, error) {
	rows, err := s.q.ListStale(ctx, dbgen.ListStaleParams{
		Actor: actor,
		ExpiresAt: sql.NullString{
			String: before.UTC().Format(time.RFC3339),
			Valid:  true,
		},
	})
	if err != nil {
		return nil, err
	}
	out := make([]Metadata, len(rows))
	for i, r := range rows {
		out[i] = *rowToMetadata(r)
	}
	return out, nil
}

func (s *Store) Delete(ctx context.Context, id ResourceID) error {
	return s.q.DeleteCacheMetadata(ctx, dbgen.DeleteCacheMetadataParams{
		Actor:        id.Actor,
		ResourceKind: id.Kind,
		ResourceKey:  id.Key,
	})
}

func (s *Store) InsertRefreshLog(ctx context.Context, id ResourceID, trigger TriggerSource) (int64, error) {
	return s.q.InsertRefreshLog(ctx, dbgen.InsertRefreshLogParams{
		Actor:        id.Actor,
		ResourceKind: id.Kind,
		ResourceKey:  id.Key,
		TriggeredBy:  string(trigger),
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Store) CompleteRefreshLog(ctx context.Context, logID int64, success bool, recordsChanged int, errMsg string) error {
	var successVal sql.NullInt64
	if success {
		successVal = sql.NullInt64{Int64: 1, Valid: true}
	} else {
		successVal = sql.NullInt64{Int64: 0, Valid: true}
	}
	return s.q.CompleteRefreshLog(ctx, dbgen.CompleteRefreshLogParams{
		CompletedAt:    sql.NullString{String: time.Now().UTC().Format(time.RFC3339), Valid: true},
		Success:        successVal,
		RecordsChanged: sql.NullInt64{Int64: int64(recordsChanged), Valid: true},
		ErrorMessage:   nullString(errMsg),
		ID:             logID,
	})
}

// helpers

func rowToMetadata(r dbgen.CacheMetadatum) *Metadata {
	m := &Metadata{
		ResourceID:   ResourceID{Kind: r.ResourceKind, Key: r.ResourceKey, Actor: r.Actor},
		ETag:         r.Etag.String,
		State:        FetchState(r.FetchState),
		ErrorMessage: r.ErrorMessage.String,
	}
	m.LastFetchedAt = parseOptionalTime(r.LastFetchedAt)
	m.LastChangedAt = parseOptionalTime(r.LastChangedAt)
	m.ExpiresAt = parseOptionalTime(r.ExpiresAt)
	m.RetryAfter = parseOptionalTime(r.RetryAfter)
	return m
}

func parseOptionalTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s.String)
	if err != nil {
		return nil
	}
	return &t
}

func optionalTime(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(time.RFC3339), Valid: true}
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
