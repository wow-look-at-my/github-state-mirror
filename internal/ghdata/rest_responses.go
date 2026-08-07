package ghdata

import (
	"context"
	"database/sql"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// RESTResponse is a cached REST body after GitHub URL/link fields have been
// stripped from JSON responses.
type RESTResponse struct {
	ResourceKind string
	ResourceKey  string
	StatusCode   int64
	ContentType  sql.NullString
	Body         []byte
	UpdatedAt    string
}

func (s *Store) UpsertRESTResponse(ctx context.Context, resp RESTResponse) error {
	return s.q.UpsertRESTResponse(ctx, dbgen.UpsertRESTResponseParams{
		Actor:        actor.FromContext(ctx),
		ResourceKind: resp.ResourceKind,
		ResourceKey:  resp.ResourceKey,
		StatusCode:   resp.StatusCode,
		ContentType:  resp.ContentType,
		Body:         resp.Body,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Store) GetRESTResponse(ctx context.Context, kind, key string) (RESTResponse, error) {
	row, err := s.q.GetRESTResponse(ctx, dbgen.GetRESTResponseParams{
		Actor:        actor.FromContext(ctx),
		ResourceKind: kind,
		ResourceKey:  key,
	})
	if err != nil {
		return RESTResponse{}, err
	}
	return RESTResponse{
		ResourceKind: row.ResourceKind,
		ResourceKey:  row.ResourceKey,
		StatusCode:   row.StatusCode,
		ContentType:  row.ContentType,
		Body:         row.Body,
		UpdatedAt:    row.UpdatedAt,
	}, nil
}

func (s *Store) MarkRESTResponsesStaleByKeyPrefix(ctx context.Context, kind, keyPrefix string) error {
	return s.q.MarkRESTResponsesStaleByKeyPrefix(ctx, dbgen.MarkRESTResponsesStaleByKeyPrefixParams{
		ResourceKind: kind,
		KeyPrefix:    sql.NullString{String: keyPrefix, Valid: true},
	})
}
