package freshness

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
)

// Manager coordinates freshness checks and delegates to registered fetchers.
type Manager struct {
	store    *Store
	policies map[string]Policy
	fetchers map[string]Fetcher
	locks    sync.Map // map[string]*sync.Mutex — per-resource lock

	// inflight tracks detached fetches so shutdown can drain them before the DB closes.
	inflight sync.WaitGroup
	// inflightCount mirrors the WaitGroup for a non-blocking read: Busy() must answer now.
	inflightCount atomic.Int64
}

func NewManager(store *Store) *Manager {
	return &Manager{
		store:    store,
		policies: make(map[string]Policy),
		fetchers: make(map[string]Fetcher),
	}
}

// Busy reports whether a detached fetch is running right now, without waiting (Drain blocks).
func (m *Manager) Busy() bool {
	if m == nil {
		return false
	}
	return m.inflightCount.Load() > 0
}

// Drain blocks until every in-flight fetch (and its metadata writes) has
// finished, or the timeout elapses. Call it during shutdown BEFORE closing the
// database. Returns true when fully drained.
func (m *Manager) Drain(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		m.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// RegisterFetcher registers a Fetcher for a resource kind with its policy.
func (m *Manager) RegisterFetcher(policy Policy, f Fetcher) {
	m.policies[policy.Kind] = policy
	m.fetchers[policy.Kind] = f
}

// Outcome is a freshness check's result: served from cache (Hit), fetched (Miss), or failed (Error).
type Outcome int

const (
	OutcomeHit Outcome = iota
	OutcomeMiss
	OutcomeError
)

// EnsureFresh fetches synchronously if stale or unknown; a thin wrapper over EnsureFreshOutcome.
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

	// An error row still inside its retry-after window is reported, not re-fetched.
	if err := backoffError(meta); err != nil {
		return OutcomeError, err
	}

	// Stale, unknown, expired, or error — need to fetch.
	return m.doFetch(ctx, id, TriggerLazy)
}

// Metadata returns stored freshness metadata for a resource (nil if never seen), read-only.
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

// fetchSafetyTimeout is a leak guard: a wedged upstream must not block shutdown's Drain forever.
const fetchSafetyTimeout = 5 * time.Minute

func (m *Manager) doFetch(ctx context.Context, id ResourceID, trigger TriggerSource) (Outcome, error) {
	fetcher, ok := m.fetchers[id.Kind]
	if !ok {
		slog.Warn("no fetcher registered", "kind", id.Kind)
		return OutcomeHit, nil
	}

	// Detached from the caller's cancellation: the fetch is shared work, so an
	// aborting client must not kill it or prevent the result from being stored
	// (see CLAUDE.md, "Lazy fetches are detached, backoff-gated, and drained").
	m.inflight.Add(1)
	m.inflightCount.Add(1)
	defer func() {
		m.inflightCount.Add(-1)
		m.inflight.Done()
	}()
	persistCtx := context.WithoutCancel(ctx)
	fetchCtx, cancelFetch := context.WithTimeout(persistCtx, fetchSafetyTimeout)
	defer cancelFetch()

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
