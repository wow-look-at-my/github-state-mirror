package freshness

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Manager coordinates freshness checks and delegates to registered fetchers.
type Manager struct {
	store    *Store
	policies map[string]Policy
	fetchers map[string]Fetcher
	locks    sync.Map // map[string]*sync.Mutex — per-resource lock
}

func NewManager(store *Store) *Manager {
	return &Manager{
		store:    store,
		policies: make(map[string]Policy),
		fetchers: make(map[string]Fetcher),
	}
}

// RegisterFetcher registers a Fetcher for a resource kind with its policy.
func (m *Manager) RegisterFetcher(policy Policy, f Fetcher) {
	m.policies[policy.Kind] = policy
	m.fetchers[policy.Kind] = f
}

// EnsureFresh checks if the resource is fresh. If stale or unknown, triggers a synchronous fetch.
func (m *Manager) EnsureFresh(ctx context.Context, id ResourceID) error {
	meta, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}

	if meta != nil && meta.State == StateFresh {
		if meta.ExpiresAt != nil && meta.ExpiresAt.After(time.Now()) {
			return nil // still fresh
		}
	}

	// Stale, unknown, expired, or error — need to fetch.
	return m.doFetch(ctx, id, TriggerLazy)
}

// Invalidate marks a resource as stale.
func (m *Manager) Invalidate(ctx context.Context, id ResourceID) error {
	meta, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if meta == nil {
		// Never seen — nothing to invalidate. It'll be lazily fetched.
		return nil
	}
	return m.store.MarkStale(ctx, id)
}

// InvalidateAndRefresh marks stale then immediately fetches.
func (m *Manager) InvalidateAndRefresh(ctx context.Context, id ResourceID, trigger TriggerSource) error {
	if err := m.Invalidate(ctx, id); err != nil {
		slog.Warn("invalidate failed", "resource", id, "error", err)
	}
	return m.doFetch(ctx, id, trigger)
}

// RefreshAllOfKind fetches all known resources of a given kind.
func (m *Manager) RefreshAllOfKind(ctx context.Context, kind string, trigger TriggerSource) error {
	metas, err := m.store.ListByKind(ctx, kind)
	if err != nil {
		return err
	}
	for _, meta := range metas {
		if err := m.doFetch(ctx, meta.ResourceID, trigger); err != nil {
			slog.Warn("refresh failed", "resource", meta.ResourceID, "error", err)
		}
	}
	return nil
}

func (m *Manager) doFetch(ctx context.Context, id ResourceID, trigger TriggerSource) error {
	fetcher, ok := m.fetchers[id.Kind]
	if !ok {
		slog.Warn("no fetcher registered", "kind", id.Kind)
		return nil
	}

	// Per-resource mutex to coalesce concurrent fetches.
	mu := m.resourceMutex(id)
	mu.Lock()
	defer mu.Unlock()

	// Re-check after acquiring lock — another goroutine may have fetched.
	if trigger == TriggerLazy {
		meta, err := m.store.Get(ctx, id)
		if err == nil && meta != nil && meta.State == StateFresh {
			if meta.ExpiresAt != nil && meta.ExpiresAt.After(time.Now()) {
				return nil
			}
		}
	}

	// Ensure metadata row exists before marking fetching.
	meta, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if meta == nil {
		if err := m.store.Upsert(ctx, &Metadata{
			ResourceID: id,
			State:      StateFetching,
		}); err != nil {
			return err
		}
	} else {
		if err := m.store.MarkFetching(ctx, id); err != nil {
			return err
		}
	}

	logID, err := m.store.InsertRefreshLog(ctx, id, trigger)
	if err != nil {
		slog.Warn("insert refresh log failed", "error", err)
	}

	etag := ""
	if meta != nil {
		etag = meta.ETag
	}

	result, fetchErr := fetcher.Fetch(ctx, id.Key, etag)

	policy := m.policies[id.Kind]
	if fetchErr != nil {
		retryAfter := time.Now().Add(policy.ErrorRetryMin)
		if policy.ErrorRetryMin == 0 {
			retryAfter = time.Now().Add(1 * time.Minute)
		}
		_ = m.store.MarkError(ctx, id, fetchErr.Error(), retryAfter)
		if logID > 0 {
			_ = m.store.CompleteRefreshLog(ctx, logID, false, 0, fetchErr.Error())
		}
		return fetchErr
	}

	ttl := policy.DefaultTTL
	if ttl == 0 {
		ttl = 6 * time.Hour
	}
	expiresAt := time.Now().Add(ttl)
	if err := m.store.MarkFresh(ctx, id, result.NewETag, expiresAt); err != nil {
		return err
	}
	if logID > 0 {
		_ = m.store.CompleteRefreshLog(ctx, logID, true, result.RecordsChanged, "")
	}

	return nil
}

func (m *Manager) resourceMutex(id ResourceID) *sync.Mutex {
	key := id.String()
	v, _ := m.locks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}
