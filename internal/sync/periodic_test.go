package sync

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
)

// recordingFetcher records every (actor, key) it was asked to fetch.
type recordingFetcher struct {
	mu      sync.Mutex
	fetched []string // "actor|key"
}

func (f *recordingFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, actor.FromContext(ctx)+"|"+key)
	return freshness.RefreshResult{RecordsChanged: 1}, nil
}

func (f *recordingFetcher) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fetched...)
}

// TestPeriodicRefresher_SyncsFreshInstallation is the fleet-sync regression
// guard: a brand-new installation session -- NO pre-seeded cache_metadata row,
// which was exactly the production state -- must be fetched on the first
// cycle, and the fetch must create its freshness marker under the session's
// actor. (The old RefreshAllOfKind shape only re-fetched rows that already
// existed, and nothing ever created one: a permanent no-op.)
func TestPeriodicRefresher_SyncsFreshInstallation(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)
	rec := &recordingFetcher{}
	mgr.RegisterFetcher(freshness.Policy{Kind: KindOrgRepos}, rec)

	sessions := func(ctx context.Context) ([]Session, error) {
		return []Session{
			{Ctx: actor.WithActor(ctx, AppInstallationActor(1)), Owner: "wow-look-at-my", AccountType: "Organization", InstallationID: 1},
			{Ctx: actor.WithActor(ctx, AppInstallationActor(2)), Owner: "PazerOP", AccountType: "User", InstallationID: 2},
		}, nil
	}
	refresher := NewPeriodicRefresher(mgr, time.Hour, sessions)
	refresher.refreshAll(context.Background())

	assert.Equal(t, []string{
		"app-installation:1|wow-look-at-my",
		"app-installation:2|PazerOP",
	}, rec.calls(), "every installation owner must be fetched, org or user, seeded or not")

	// The fetch itself created the freshness marker under the session's actor.
	meta, err := fStore.Get(context.Background(), freshness.ResourceID{
		Kind: KindOrgRepos, Key: "PazerOP", Actor: AppInstallationActor(2),
	})
	require.NoError(t, err)
	require.NotNil(t, meta, "doFetch must seed the cache_metadata row itself")
	assert.Equal(t, freshness.StateFresh, meta.State)
}

// TestPeriodicRefresher_StartRunsImmediately: Start's FIRST fleet refresh runs
// at startup, not one full interval in. The interval here is an hour, so any
// fetch observed within the test window can only be the startup cycle -- the
// production regression was exactly a bare ticker whose first fire sat 6h
// away while near-daily schema-bump deploys (which also nuke the freshness
// markers) restarted the clock, so no cycle ever completed.
func TestPeriodicRefresher_StartRunsImmediately(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)
	rec := &recordingFetcher{}
	mgr.RegisterFetcher(freshness.Policy{Kind: KindOrgRepos}, rec)

	sessions := func(ctx context.Context) ([]Session, error) {
		return []Session{
			{Ctx: actor.WithActor(ctx, AppInstallationActor(1)), Owner: "wow-look-at-my", AccountType: "Organization", InstallationID: 1},
			{Ctx: actor.WithActor(ctx, AppInstallationActor(2)), Owner: "PazerOP", AccountType: "User", InstallationID: 2},
		}, nil
	}
	refresher := NewPeriodicRefresher(mgr, time.Hour, sessions)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		refresher.Start(ctx)
		close(done)
	}()
	require.Eventually(t, func() bool { return len(rec.calls()) == 2 },
		5*time.Second, 10*time.Millisecond, "the startup cycle must fetch every session without waiting for a tick")
	cancel()
	<-done

	// The startup cycle stamped a freshness row for EVERY enumerated session,
	// org and User account alike, under its app-installation principal.
	for owner, install := range map[string]int64{"wow-look-at-my": 1, "PazerOP": 2} {
		meta, err := fStore.Get(context.Background(), freshness.ResourceID{
			Kind: KindOrgRepos, Key: owner, Actor: AppInstallationActor(install),
		})
		require.NoError(t, err)
		require.NotNil(t, meta, "owner %s must have a freshness row after the startup cycle", owner)
		assert.Equal(t, freshness.StateFresh, meta.State, "owner %s", owner)
	}
}

// failingFetcher records like recordingFetcher but errors for one key.
type failingFetcher struct {
	recordingFetcher
	failKey string
}

func (f *failingFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	_, _ = f.recordingFetcher.Fetch(ctx, key, etag)
	if key == f.failKey {
		return freshness.RefreshResult{}, errors.New("github api POST /graphql: 502 Bad Gateway")
	}
	return freshness.RefreshResult{RecordsChanged: 1}, nil
}

// TestPeriodicRefresher_FailingOwnerStillMarksOthers: one owner's failed fetch
// must leave a visible error marker (state error + the message) and must not
// stop the rest of the fleet from syncing fresh.
func TestPeriodicRefresher_FailingOwnerStillMarksOthers(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)
	rec := &failingFetcher{failKey: "badorg"}
	mgr.RegisterFetcher(freshness.Policy{Kind: KindOrgRepos}, rec)

	sessions := func(ctx context.Context) ([]Session, error) {
		return []Session{
			{Ctx: actor.WithActor(ctx, AppInstallationActor(1)), Owner: "badorg", InstallationID: 1},
			{Ctx: actor.WithActor(ctx, AppInstallationActor(2)), Owner: "goodorg", InstallationID: 2},
		}, nil
	}
	NewPeriodicRefresher(mgr, time.Hour, sessions).refreshAll(context.Background())

	bad, err := fStore.Get(context.Background(), freshness.ResourceID{
		Kind: KindOrgRepos, Key: "badorg", Actor: AppInstallationActor(1),
	})
	require.NoError(t, err)
	require.NotNil(t, bad, "a failed fetch still seeds its marker row")
	assert.Equal(t, freshness.StateError, bad.State)
	assert.Contains(t, bad.ErrorMessage, "502 Bad Gateway")

	good, err := fStore.Get(context.Background(), freshness.ResourceID{
		Kind: KindOrgRepos, Key: "goodorg", Actor: AppInstallationActor(2),
	})
	require.NoError(t, err)
	require.NotNil(t, good)
	assert.Equal(t, freshness.StateFresh, good.State, "one owner's failure must not block the rest")
}

// TestPeriodicRefresher_BypassesErrorBackoff: a deliberate periodic refresh
// re-fetches even inside a previous failure's retry-after window (only lazy
// callers honor the backoff).
func TestPeriodicRefresher_BypassesErrorBackoff(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)
	rec := &recordingFetcher{}
	mgr.RegisterFetcher(freshness.Policy{Kind: KindOrgRepos}, rec)

	id := freshness.ResourceID{Kind: KindOrgRepos, Key: "org1", Actor: AppInstallationActor(9)}
	retryAfter := time.Now().Add(time.Hour)
	require.NoError(t, fStore.Upsert(ctxMeta(id)))
	require.NoError(t, fStore.MarkError(context.Background(), id, "boom", retryAfter))

	sessions := func(ctx context.Context) ([]Session, error) {
		return []Session{{Ctx: actor.WithActor(ctx, AppInstallationActor(9)), Owner: "org1", InstallationID: 9}}, nil
	}
	NewPeriodicRefresher(mgr, time.Hour, sessions).refreshAll(context.Background())

	assert.Len(t, rec.calls(), 1, "TriggerPeriodic must bypass the lazy error backoff")
}

// ctxMeta packs an Upsert call's arguments (helper keeps the test terse).
func ctxMeta(id freshness.ResourceID) (context.Context, *freshness.Metadata) {
	return context.Background(), &freshness.Metadata{ResourceID: id, State: freshness.StateUnknown}
}

func TestPeriodicRefresher_Start(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)
	t.Cleanup(func() { db.Close() })

	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)
	mgr.RegisterFetcher(freshness.Policy{Kind: KindOrgRepos}, &stubFetcher{})

	sessions := func(ctx context.Context) ([]Session, error) {
		return []Session{{Ctx: ctx, Owner: "org1"}}, nil
	}
	refresher := NewPeriodicRefresher(mgr, 50*time.Millisecond, sessions)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	refresher.Start(ctx)

	// If we get here without hanging, the context cancellation worked.
	assert.True(t, true)
}
