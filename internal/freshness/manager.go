package freshness

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
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

// Outcome reports whether a freshness check served the resource from cache
// (Hit), triggered an upstream fetch (Miss), or failed (Error). It lets the API
// layer record per-request cache dispositions for the dashboard.
type Outcome int

const (
	OutcomeHit Outcome = iota
	OutcomeMiss
	OutcomeError
)

// EnsureFresh checks if the resource is fresh. If stale or unknown, triggers a
// synchronous fetch. It is a thin wrapper over EnsureFreshOutcome for callers
// that don't need the hit/miss outcome.
func (m *Manager) EnsureFresh(ctx context.Context, id ResourceID) error {
	_, err := m.EnsureFreshOutcome(ctx, id)
	return err
}

// EnsureFreshOutcome is EnsureFresh that also reports the cache outcome: Hit when
// the resource was already fresh (no upstream call), Miss when a fetch was
// triggered, Error on failure.
func (m *Manager) EnsureFreshOutcome(ctx context.Context, id ResourceID) (Outcome, error) {
	id = m.fillActor(ctx, id)

	meta, err := m.store.Get(ctx, id)
	if err != nil {
		return OutcomeError, err
	}

	if meta != nil && meta.State == StateFresh {
		if meta.ExpiresAt != nil && meta.ExpiresAt.After(time.Now()) {
			return OutcomeHit, nil // still fresh
		}
	}

	// An error-state row still inside its retry-after window: do NOT re-attempt
	// the fetch — a failing upstream would otherwise be hammered with a full
	// (expensive, all-or-nothing) fetch on every request. Report the stored
	// error; callers with cached data serve it stale.
	if err := backoffError(meta); err != nil {
		return OutcomeError, err
	}

	// Stale, unknown, expired, or error — need to fetch.
	return m.doFetch(ctx, id, TriggerLazy)
}

// Metadata returns the stored freshness metadata for a resource (nil when the
// resource has never been seen). Read-only — lets API handlers surface
// staleness (last-fetched time, error state) without reaching into the store.
func (m *Manager) Metadata(ctx context.Context, id ResourceID) (*Metadata, error) {
	return m.store.Get(ctx, m.fillActor(ctx, id))
}

// backoffError returns a non-nil error when meta is an error-state row whose
// retry-after moment has not yet arrived — the fetch must not be re-attempted —
// carrying the stored upstream error. Nil otherwise (including nil meta).
func backoffError(meta *Metadata) error {
	if meta == nil || meta.State != StateError || meta.RetryAfter == nil || !time.Now().Before(*meta.RetryAfter) {
		return nil
	}
	msg := meta.ErrorMessage
	if msg == "" {
		msg = "unknown error"
	}
	return fmt.Errorf("upstream fetch failed, in retry backoff until %s: %s",
		meta.RetryAfter.UTC().Format(time.RFC3339), msg)
}

// Invalidate marks a resource as stale.
func (m *Manager) Invalidate(ctx context.Context, id ResourceID) error {
	id = m.fillActor(ctx, id)

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

// InvalidateAllActors marks a resource as stale for all actors.
// Used by webhooks where the change affects everyone's cache.
func (m *Manager) InvalidateAllActors(ctx context.Context, kind, key string) error {
	return m.store.MarkStaleAllActors(ctx, kind, key)
}

// InvalidateAndRefresh marks stale then immediately fetches.
func (m *Manager) InvalidateAndRefresh(ctx context.Context, id ResourceID, trigger TriggerSource) error {
	id = m.fillActor(ctx, id)

	if err := m.Invalidate(ctx, id); err != nil {
		slog.Warn("invalidate failed", "resource", id, "error", err)
	}
	_, err := m.doFetch(ctx, id, trigger)
	return err
}

// RefreshAllOfKind fetches all known resources of a given kind for the current actor.
func (m *Manager) RefreshAllOfKind(ctx context.Context, kind string, trigger TriggerSource) error {
	act := actor.FromContext(ctx)
	metas, err := m.store.ListByKind(ctx, act, kind)
	if err != nil {
		return err
	}
	for _, meta := range metas {
		if _, err := m.doFetch(ctx, meta.ResourceID, trigger); err != nil {
			slog.Warn("refresh failed", "resource", meta.ResourceID, "error", err)
		}
	}
	return nil
}

func (m *Manager) doFetch(ctx context.Context, id ResourceID, trigger TriggerSource) (Outcome, error) {
	fetcher, ok := m.fetchers[id.Kind]
	if !ok {
		slog.Warn("no fetcher registered", "kind", id.Kind)
		return OutcomeHit, nil
	}

	// Detach the fetch from the caller's cancellation. The fetch is shared work
	// — its result is cached for every future caller — so an impatient client
	// aborting its request mid-flight must not kill a multi-page all-or-nothing
	// fetch (previously a browser abort could prevent a scope from EVER
	// refreshing), and the result must be stored even when the requester is
	// gone. Context values (actor, auth token, tracing) are preserved.
	//
	// persistCtx: never canceled — metadata writes always land.
	// fetchCtx: cancel-severed too, but re-applies the caller's explicit
	// deadline (e.g. the webhook dispatch timeout) so bounded callers stay
	// bounded.
	persistCtx := context.WithoutCancel(ctx)
	fetchCtx := persistCtx
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		fetchCtx, cancel = context.WithDeadline(persistCtx, deadline)
		defer cancel()
	}

	// Per-resource mutex to coalesce concurrent fetches.
	mu := m.resourceMutex(id)
	mu.Lock()
	defer mu.Unlock()

	// Re-check after acquiring lock — another goroutine may have fetched, or
	// just failed (in which case honor its retry-after backoff instead of
	// retrying immediately from the pile-up behind the lock).
	if trigger == TriggerLazy {
		meta, err := m.store.Get(persistCtx, id)
		if err == nil && meta != nil {
			if meta.State == StateFresh && meta.ExpiresAt != nil && meta.ExpiresAt.After(time.Now()) {
				return OutcomeHit, nil
			}
			if err := backoffError(meta); err != nil {
				return OutcomeError, err
			}
		}
	}

	// Ensure metadata row exists before marking fetching.
	meta, err := m.store.Get(persistCtx, id)
	if err != nil {
		return OutcomeError, err
	}
	if meta == nil {
		if err := m.store.Upsert(persistCtx, &Metadata{
			ResourceID: id,
			State:      StateFetching,
		}); err != nil {
			return OutcomeError, err
		}
	} else {
		if err := m.store.MarkFetching(persistCtx, id); err != nil {
			return OutcomeError, err
		}
	}

	logID, err := m.store.InsertRefreshLog(persistCtx, id, trigger)
	if err != nil {
		slog.Warn("insert refresh log failed", "error", err)
	}

	etag := ""
	if meta != nil {
		etag = meta.ETag
	}

	result, fetchErr := fetcher.Fetch(fetchCtx, id.Key, etag)

	policy := m.policies[id.Kind]
	if fetchErr != nil {
		retryAfter := time.Now().Add(policy.ErrorRetryMin)
		if policy.ErrorRetryMin == 0 {
			retryAfter = time.Now().Add(1 * time.Minute)
		}
		_ = m.store.MarkError(persistCtx, id, fetchErr.Error(), retryAfter)
		if logID > 0 {
			_ = m.store.CompleteRefreshLog(persistCtx, logID, false, 0, fetchErr.Error())
		}
		return OutcomeError, fetchErr
	}

	ttl := policy.DefaultTTL
	if ttl == 0 {
		ttl = 6 * time.Hour
	}
	expiresAt := time.Now().Add(ttl)
	if err := m.store.MarkFresh(persistCtx, id, result.NewETag, expiresAt); err != nil {
		return OutcomeError, err
	}
	if logID > 0 {
		_ = m.store.CompleteRefreshLog(persistCtx, logID, true, result.RecordsChanged, "")
	}

	return OutcomeMiss, nil
}

func (m *Manager) fillActor(ctx context.Context, id ResourceID) ResourceID {
	if id.Actor == "" {
		id.Actor = actor.FromContext(ctx)
	}
	return id
}

func (m *Manager) resourceMutex(id ResourceID) *sync.Mutex {
	key := id.String()
	v, _ := m.locks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}
