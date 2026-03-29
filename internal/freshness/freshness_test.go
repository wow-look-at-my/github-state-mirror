package freshness

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func TestStore_GetNonExistent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m, err := s.Get(ctx, ResourceID{Kind: "test", Key: "missing"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil for non-existent resource, got %+v", m)
	}
}

func TestStore_UpsertAndGet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	id := ResourceID{Kind: "test", Key: "key1"}
	m := &Metadata{
		ResourceID: id,
		State:      StateFresh,
		ETag:       "etag123",
		ExpiresAt:  &now,
	}
	if err := s.Upsert(ctx, m); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.State != StateFresh {
		t.Errorf("state = %q, want %q", got.State, StateFresh)
	}
	if got.ETag != "etag123" {
		t.Errorf("etag = %q, want %q", got.ETag, "etag123")
	}
}

func TestStore_MarkFreshAndStale(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := ResourceID{Kind: "test", Key: "key1"}
	if err := s.Upsert(ctx, &Metadata{ResourceID: id, State: StateUnknown}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	expires := time.Now().Add(1 * time.Hour)
	if err := s.MarkFresh(ctx, id, "etag456", expires); err != nil {
		t.Fatalf("MarkFresh: %v", err)
	}

	got, _ := s.Get(ctx, id)
	if got.State != StateFresh {
		t.Errorf("state = %q, want %q", got.State, StateFresh)
	}

	if err := s.MarkStale(ctx, id); err != nil {
		t.Fatalf("MarkStale: %v", err)
	}

	got, _ = s.Get(ctx, id)
	if got.State != StateStale {
		t.Errorf("state = %q, want %q", got.State, StateStale)
	}
}

func TestStore_MarkError(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := ResourceID{Kind: "test", Key: "key1"}
	if err := s.Upsert(ctx, &Metadata{ResourceID: id, State: StateUnknown}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	retryAt := time.Now().Add(5 * time.Minute)
	if err := s.MarkError(ctx, id, "connection refused", retryAt); err != nil {
		t.Fatalf("MarkError: %v", err)
	}

	got, _ := s.Get(ctx, id)
	if got.State != StateError {
		t.Errorf("state = %q, want %q", got.State, StateError)
	}
	if got.ErrorMessage != "connection refused" {
		t.Errorf("error = %q, want %q", got.ErrorMessage, "connection refused")
	}
}

func TestStore_ListByKind(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, key := range []string{"a", "b", "c"} {
		if err := s.Upsert(ctx, &Metadata{
			ResourceID: ResourceID{Kind: "repos", Key: key},
			State:      StateFresh,
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	// Different kind.
	if err := s.Upsert(ctx, &Metadata{
		ResourceID: ResourceID{Kind: "users", Key: "x"},
		State:      StateFresh,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	metas, err := s.ListByKind(ctx, "repos")
	if err != nil {
		t.Fatalf("ListByKind: %v", err)
	}
	if len(metas) != 3 {
		t.Errorf("len = %d, want 3", len(metas))
	}
}

func TestStore_RefreshLog(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := ResourceID{Kind: "test", Key: "key1"}
	logID, err := s.InsertRefreshLog(ctx, id, TriggerLazy)
	if err != nil {
		t.Fatalf("InsertRefreshLog: %v", err)
	}
	if logID <= 0 {
		t.Errorf("logID = %d, want > 0", logID)
	}

	if err := s.CompleteRefreshLog(ctx, logID, true, 5, ""); err != nil {
		t.Fatalf("CompleteRefreshLog: %v", err)
	}
}

func TestManager_EnsureFresh_LazyFetch(t *testing.T) {
	s := testStore(t)
	mgr := NewManager(s)

	fetchCount := 0
	mgr.RegisterFetcher(Policy{
		Kind:       "test",
		DefaultTTL: 1 * time.Hour,
	}, FetcherFunc(func(ctx context.Context, key string, etag string) (RefreshResult, error) {
		fetchCount++
		return RefreshResult{RecordsChanged: 1}, nil
	}))

	ctx := context.Background()
	id := ResourceID{Kind: "test", Key: "key1"}

	// First call should trigger a fetch.
	if err := mgr.EnsureFresh(ctx, id); err != nil {
		t.Fatalf("EnsureFresh 1: %v", err)
	}
	if fetchCount != 1 {
		t.Errorf("fetchCount = %d, want 1", fetchCount)
	}

	// Second call — data is fresh, no fetch.
	if err := mgr.EnsureFresh(ctx, id); err != nil {
		t.Fatalf("EnsureFresh 2: %v", err)
	}
	if fetchCount != 1 {
		t.Errorf("fetchCount = %d, want 1 (should not re-fetch)", fetchCount)
	}
}

func TestManager_Invalidate_ThenEnsureFresh(t *testing.T) {
	s := testStore(t)
	mgr := NewManager(s)

	fetchCount := 0
	mgr.RegisterFetcher(Policy{
		Kind:       "test",
		DefaultTTL: 1 * time.Hour,
	}, FetcherFunc(func(ctx context.Context, key string, etag string) (RefreshResult, error) {
		fetchCount++
		return RefreshResult{RecordsChanged: 1}, nil
	}))

	ctx := context.Background()
	id := ResourceID{Kind: "test", Key: "key1"}

	// Fetch once.
	mgr.EnsureFresh(ctx, id)

	// Invalidate.
	if err := mgr.Invalidate(ctx, id); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	// EnsureFresh should re-fetch.
	if err := mgr.EnsureFresh(ctx, id); err != nil {
		t.Fatalf("EnsureFresh after invalidate: %v", err)
	}
	if fetchCount != 2 {
		t.Errorf("fetchCount = %d, want 2", fetchCount)
	}
}

// FetcherFunc is an adapter to use ordinary functions as Fetcher.
type FetcherFunc func(ctx context.Context, key string, etag string) (RefreshResult, error)

func (f FetcherFunc) Fetch(ctx context.Context, key string, etag string) (RefreshResult, error) {
	return f(ctx, key, etag)
}
