package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Storage for the Actions WORKFLOW definitions. A definition is not a run: it
// changes only when a push edits .github/workflows, which is why both grains
// share a table and a repo-wide flush.

const (
	// The grains: the repo's listing, and a single workflow.
	WorkflowsKindList   = "list"
	WorkflowsKindSingle = "single"

	// WorkflowsCacheTTL backstops a lost push delivery.
	WorkflowsCacheTTL = 6 * time.Hour
)

// WorkflowsKey addresses a stored workflows answer.
type WorkflowsKey struct {
	Owner   string
	Repo    string
	Kind    string // WorkflowsKind*
	RefID   string // single: the requested id or file name
	PerPage int64  // list paging
	Page    int64
}

func (k WorkflowsKey) get() dbgen.GetWorkflowsCacheParams {
	return dbgen.GetWorkflowsCacheParams{
		Owner: NormalizeRepoKey(k.Owner), Repo: NormalizeRepoKey(k.Repo),
		Kind: k.Kind, RefID: k.RefID, PerPage: k.PerPage, Page: k.Page,
	}
}

// GetCachedWorkflows returns the stored document, or false on a miss. A hit
// refreshes the row's LRU stamp.
func (s *Store) GetCachedWorkflows(ctx context.Context, k WorkflowsKey, now time.Time) (string, bool, error) {
	row, err := s.q.GetWorkflowsCache(ctx, k.get())
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if exp, perr := time.Parse(time.RFC3339, row.ExpiresAt); perr != nil || !exp.After(now) {
		return "", false, nil
	}
	_ = s.q.TouchWorkflowsCache(ctx, dbgen.TouchWorkflowsCacheParams{
		LastUsedAt: rfc3339(now), Owner: NormalizeRepoKey(k.Owner), Repo: NormalizeRepoKey(k.Repo),
		Kind: k.Kind, RefID: k.RefID, PerPage: k.PerPage, Page: k.Page,
	})
	return row.Doc, true, nil
}

// PutCachedWorkflows records a fetched answer, then prunes expired rows and
// the LRU tail beyond the cap.
func (s *Store) PutCachedWorkflows(ctx context.Context, k WorkflowsKey, doc string, now time.Time, ttl time.Duration) error {
	if err := s.q.UpsertWorkflowsCache(ctx, dbgen.UpsertWorkflowsCacheParams{
		Owner: NormalizeRepoKey(k.Owner), Repo: NormalizeRepoKey(k.Repo),
		Kind: k.Kind, RefID: k.RefID, PerPage: k.PerPage, Page: k.Page, Doc: doc,
		FetchedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(ttl)), LastUsedAt: rfc3339(now),
	}); err != nil {
		return err
	}
	if err := s.q.DeleteExpiredWorkflowsCache(ctx, rfc3339(now)); err != nil {
		return err
	}
	return s.q.PruneWorkflowsCacheLRU(ctx, CacheMaxRows)
}

// InvalidateWorkflowsCache drops a repo's cached workflow answers, both grains.
func (s *Store) InvalidateWorkflowsCache(ctx context.Context, owner, repo string) error {
	return s.q.DeleteWorkflowsCacheByRepo(ctx, dbgen.DeleteWorkflowsCacheByRepoParams{
		Owner: NormalizeRepoKey(owner), Repo: NormalizeRepoKey(repo),
	})
}
