package freshness

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
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
		ResourceID: id,
		State:      StateFresh,
		ETag:       "etag123",
		ExpiresAt:  &now,
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

	metas, err := s.ListByKind(ctx, "", "repos")
	require.Nil(t, err)

	assert.Equal(t, 3, len(metas))

}

func TestStore_RefreshLog(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := ResourceID{Kind: "test", Key: "key1"}
	logID, err := s.InsertRefreshLog(ctx, id, TriggerLazy)
	require.Nil(t, err)

	assert.Greater(t, logID, int64(0))

	require.NoError(t, s.CompleteRefreshLog(ctx, logID, true, 5, ""))

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
	require.NoError(t, mgr.Invalidate(ctx, id))

	// EnsureFresh should re-fetch.
	require.NoError(t, mgr.EnsureFresh(ctx, id))

	assert.Equal(t, 2, fetchCount)

}

func TestManager_InvalidateAndRefresh(t *testing.T) {
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

	// Initial fetch.
	require.NoError(t, mgr.EnsureFresh(ctx, id))
	assert.Equal(t, 1, fetchCount)

	// InvalidateAndRefresh should re-fetch immediately.
	require.NoError(t, mgr.InvalidateAndRefresh(ctx, id, TriggerWebhook))
	assert.Equal(t, 2, fetchCount)

	// Should be fresh now.
	meta, err := s.Get(ctx, id)
	require.Nil(t, err)
	assert.Equal(t, StateFresh, meta.State)
}

func TestManager_RefreshAllOfKind(t *testing.T) {
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

	// Seed a few resources.
	require.NoError(t, mgr.EnsureFresh(ctx, ResourceID{Kind: "test", Key: "a"}))
	require.NoError(t, mgr.EnsureFresh(ctx, ResourceID{Kind: "test", Key: "b"}))
	assert.Equal(t, 2, fetchCount)

	// RefreshAllOfKind should re-fetch both.
	require.NoError(t, mgr.RefreshAllOfKind(ctx, "test", TriggerPeriodic))
	assert.Equal(t, 4, fetchCount)
}

func TestManager_FetchError(t *testing.T) {
	s := testStore(t)
	mgr := NewManager(s)

	mgr.RegisterFetcher(Policy{
		Kind:          "test",
		DefaultTTL:    1 * time.Hour,
		ErrorRetryMin: 5 * time.Minute,
	}, FetcherFunc(func(ctx context.Context, key string, etag string) (RefreshResult, error) {
		return RefreshResult{}, assert.AnError
	}))

	ctx := context.Background()
	id := ResourceID{Kind: "test", Key: "key1"}

	err := mgr.EnsureFresh(ctx, id)
	assert.Error(t, err)

	meta, err := s.Get(ctx, id)
	require.Nil(t, err)
	assert.Equal(t, StateError, meta.State)
	assert.NotEmpty(t, meta.ErrorMessage)
}

func TestManager_InvalidateNeverSeen(t *testing.T) {
	s := testStore(t)
	mgr := NewManager(s)

	mgr.RegisterFetcher(Policy{Kind: "test"}, FetcherFunc(func(ctx context.Context, key string, etag string) (RefreshResult, error) {
		return RefreshResult{}, nil
	}))

	ctx := context.Background()
	// Invalidate a resource that was never fetched — should be a no-op.
	require.NoError(t, mgr.Invalidate(ctx, ResourceID{Kind: "test", Key: "never-seen"}))
}

func TestManager_NoFetcher(t *testing.T) {
	s := testStore(t)
	mgr := NewManager(s)

	ctx := context.Background()
	// EnsureFresh for unregistered kind — should not error.
	require.NoError(t, mgr.EnsureFresh(ctx, ResourceID{Kind: "unregistered", Key: "key1"}))
}

func TestStore_ListStale(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id1 := ResourceID{Kind: "test", Key: "stale1"}
	id2 := ResourceID{Kind: "test", Key: "fresh1"}
	require.NoError(t, s.Upsert(ctx, &Metadata{ResourceID: id1, State: StateStale}))
	require.NoError(t, s.Upsert(ctx, &Metadata{ResourceID: id2, State: StateFresh}))

	stale, err := s.ListStale(ctx, "", time.Now().Add(1*time.Hour))
	require.Nil(t, err)
	assert.Equal(t, 1, len(stale))
	assert.Equal(t, "stale1", stale[0].Key)
}

func TestStore_Delete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := ResourceID{Kind: "test", Key: "to-delete"}
	require.NoError(t, s.Upsert(ctx, &Metadata{ResourceID: id, State: StateFresh}))

	got, err := s.Get(ctx, id)
	require.Nil(t, err)
	require.NotNil(t, got)

	require.NoError(t, s.Delete(ctx, id))

	got, err = s.Get(ctx, id)
	require.Nil(t, err)
	assert.Nil(t, got)
}

// TestManager_FetchSurvivesCallerCancel locks the detached-fetch behavior: an
// in-flight fetch is shared work whose result is cached for every future
// caller, so a client aborting its request mid-fetch must neither cancel the
// fetch nor prevent the result from being stored. (Previously an impatient
// browser abort could cancel the multi-page org-repos fetch on every attempt,
// preventing a scope from ever refreshing.)
func TestManager_FetchSurvivesCallerCancel(t *testing.T) {
	s := testStore(t)
	mgr := NewManager(s)

	fetchStarted := make(chan struct{})
	proceed := make(chan struct{})
	var fetchCtxErr error
	mgr.RegisterFetcher(Policy{
		Kind:       "test",
		DefaultTTL: 1 * time.Hour,
	}, FetcherFunc(func(ctx context.Context, key string, etag string) (RefreshResult, error) {
		close(fetchStarted)
		<-proceed // hold the fetch until the caller's context has been canceled
		fetchCtxErr = ctx.Err()
		return RefreshResult{RecordsChanged: 1}, nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	id := ResourceID{Kind: "test", Key: "key1"}
	done := make(chan error, 1)
	go func() { done <- mgr.EnsureFresh(ctx, id) }()

	<-fetchStarted
	cancel() // the client aborts mid-fetch
	close(proceed)

	require.NoError(t, <-done, "the fetch must complete despite the caller's cancellation")
	assert.NoError(t, fetchCtxErr, "the fetcher's context must not be canceled by the caller's abort")

	meta, err := s.Get(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, meta)
	assert.Equal(t, StateFresh, meta.State, "the result must be stored even though the requester is gone")
}

// TestManager_FetchKeepsCallerDeadline: severing cancellation must not unbound
// bounded callers — an explicit deadline (e.g. the webhook dispatch timeout)
// still applies to the fetch context.
func TestManager_FetchKeepsCallerDeadline(t *testing.T) {
	s := testStore(t)
	mgr := NewManager(s)

	var hadDeadline bool
	mgr.RegisterFetcher(Policy{
		Kind:       "test",
		DefaultTTL: 1 * time.Hour,
	}, FetcherFunc(func(ctx context.Context, key string, etag string) (RefreshResult, error) {
		_, hadDeadline = ctx.Deadline()
		return RefreshResult{}, nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	require.NoError(t, mgr.EnsureFresh(ctx, ResourceID{Kind: "test", Key: "key1"}))
	assert.True(t, hadDeadline, "the caller's deadline must be preserved on the fetch context")
}

// TestManager_ErrorBackoffSkipsRefetch: an error-state row still inside its
// retry-after window must not re-attempt the fetch on every request (retry
// storm against a failing upstream); the stored error is reported so callers
// can deliberately serve stale data. Once the window passes, fetching resumes.
func TestManager_ErrorBackoffSkipsRefetch(t *testing.T) {
	s := testStore(t)
	mgr := NewManager(s)

	fetchCount := 0
	mgr.RegisterFetcher(Policy{
		Kind:          "test",
		DefaultTTL:    1 * time.Hour,
		ErrorRetryMin: 5 * time.Minute,
	}, FetcherFunc(func(ctx context.Context, key string, etag string) (RefreshResult, error) {
		fetchCount++
		return RefreshResult{}, assert.AnError
	}))

	ctx := context.Background()
	id := ResourceID{Kind: "test", Key: "key1"}

	// Real attempt: fails and opens a 5-minute retry window.
	err := mgr.EnsureFresh(ctx, id)
	assert.Error(t, err)
	assert.Equal(t, 1, fetchCount)

	// Within the window: no new attempt, but the stored error is still reported.
	outcome, err := mgr.EnsureFreshOutcome(ctx, id)
	assert.Error(t, err, "the stored error must still surface to callers")
	assert.Equal(t, OutcomeError, outcome)
	assert.Equal(t, 1, fetchCount, "no refetch within the retry-after window")
	assert.Contains(t, err.Error(), assert.AnError.Error(), "the error must carry the stored upstream failure")

	// A different resource kind whose window expires immediately: fetching resumes.
	fetch2 := 0
	mgr.RegisterFetcher(Policy{
		Kind:          "test2",
		DefaultTTL:    1 * time.Hour,
		ErrorRetryMin: 1 * time.Nanosecond,
	}, FetcherFunc(func(ctx context.Context, key string, etag string) (RefreshResult, error) {
		fetch2++
		return RefreshResult{}, assert.AnError
	}))
	id2 := ResourceID{Kind: "test2", Key: "key1"}
	assert.Error(t, mgr.EnsureFresh(ctx, id2))
	time.Sleep(5 * time.Millisecond) // let the nanosecond window lapse
	assert.Error(t, mgr.EnsureFresh(ctx, id2))
	assert.Equal(t, 2, fetch2, "a lapsed retry-after window must allow a re-attempt")
}

// FetcherFunc is an adapter to use ordinary functions as Fetcher.
type FetcherFunc func(ctx context.Context, key string, etag string) (RefreshResult, error)

func (f FetcherFunc) Fetch(ctx context.Context, key string, etag string) (RefreshResult, error) {
	return f(ctx, key, etag)
}
