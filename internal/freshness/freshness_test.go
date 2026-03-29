package freshness

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)

	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func TestStore_GetNonExistent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m, err := s.Get(ctx, ResourceID{Kind: "test", Key: "missing"})
	require.Nil(t, err)

	assert.Nil(t, m)

}

func TestStore_UpsertAndGet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	id := ResourceID{Kind: "test", Key: "key1"}
	m := &Metadata{
		ResourceID:	id,
		State:		StateFresh,
		ETag:		"etag123",
		ExpiresAt:	&now,
	}
	require.NoError(t, s.Upsert(ctx, m))

	got, err := s.Get(ctx, id)
	require.Nil(t, err)

	require.NotNil(t, got)

	assert.Equal(t, StateFresh, got.State)

	assert.Equal(t, "etag123", got.ETag)

}

func TestStore_MarkFreshAndStale(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := ResourceID{Kind: "test", Key: "key1"}
	require.NoError(t, s.Upsert(ctx, &Metadata{ResourceID: id, State: StateUnknown}))

	expires := time.Now().Add(1 * time.Hour)
	require.NoError(t, s.MarkFresh(ctx, id, "etag456", expires))

	got, _ := s.Get(ctx, id)
	assert.Equal(t, StateFresh, got.State)

	require.NoError(t, s.MarkStale(ctx, id))

	got, _ = s.Get(ctx, id)
	assert.Equal(t, StateStale, got.State)

}

func TestStore_MarkError(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := ResourceID{Kind: "test", Key: "key1"}
	require.NoError(t, s.Upsert(ctx, &Metadata{ResourceID: id, State: StateUnknown}))

	retryAt := time.Now().Add(5 * time.Minute)
	require.NoError(t, s.MarkError(ctx, id, "connection refused", retryAt))

	got, _ := s.Get(ctx, id)
	assert.Equal(t, StateError, got.State)

	assert.Equal(t, "connection refused", got.ErrorMessage)

}

func TestStore_ListByKind(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, key := range []string{"a", "b", "c"} {
		require.NoError(t, s.Upsert(ctx, &Metadata{ResourceID: ResourceID{Kind: "repos", Key: key}, State: StateFresh}))

	}
	// Different kind.
	require.NoError(t, s.Upsert(ctx, &Metadata{ResourceID: ResourceID{Kind: "users", Key: "x"}, State: StateFresh}))

	metas, err := s.ListByKind(ctx, "repos")
	require.Nil(t, err)

	assert.Equal(t, 3, len(metas))

}

func TestStore_RefreshLog(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := ResourceID{Kind: "test", Key: "key1"}
	logID, err := s.InsertRefreshLog(ctx, id, TriggerLazy)
	require.Nil(t, err)

	assert.Greater(t, logID, 0)

	require.NoError(t, s.CompleteRefreshLog(ctx, logID, true, 5, ""))

}

func TestManager_EnsureFresh_LazyFetch(t *testing.T) {
	s := testStore(t)
	mgr := NewManager(s)

	fetchCount := 0
	mgr.RegisterFetcher(Policy{
		Kind:		"test",
		DefaultTTL:	1 * time.Hour,
	}, FetcherFunc(func(ctx context.Context, key string, etag string) (RefreshResult, error) {
		fetchCount++
		return RefreshResult{RecordsChanged: 1}, nil
	}))

	ctx := context.Background()
	id := ResourceID{Kind: "test", Key: "key1"}

	// First call should trigger a fetch.
	require.NoError(t, mgr.EnsureFresh(ctx, id))

	assert.Equal(t, 1, fetchCount)

	// Second call — data is fresh, no fetch.
	require.NoError(t, mgr.EnsureFresh(ctx, id))

	assert.Equal(t, 1, fetchCount)

}

func TestManager_Invalidate_ThenEnsureFresh(t *testing.T) {
	s := testStore(t)
	mgr := NewManager(s)

	fetchCount := 0
	mgr.RegisterFetcher(Policy{
		Kind:		"test",
		DefaultTTL:	1 * time.Hour,
	}, FetcherFunc(func(ctx context.Context, key string, etag string) (RefreshResult, error) {
		fetchCount++
		return RefreshResult{RecordsChanged: 1}, nil
	}))

	ctx := context.Background()
	id := ResourceID{Kind: "test", Key: "key1"}

	// Fetch once.
	mgr.EnsureFresh(ctx, id)

	// Invalidate.
	require.NoError(t, mgr.Invalidate(ctx, id))

	// EnsureFresh should re-fetch.
	require.NoError(t, mgr.EnsureFresh(ctx, id))

	assert.Equal(t, 2, fetchCount)

}

// FetcherFunc is an adapter to use ordinary functions as Fetcher.
type FetcherFunc func(ctx context.Context, key string, etag string) (RefreshResult, error)

func (f FetcherFunc) Fetch(ctx context.Context, key string, etag string) (RefreshResult, error) {
	return f(ctx, key, etag)
}
