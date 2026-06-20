package sync

import (
	"context"
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

// recordingFetcher records every (actor, key) it is asked to fetch.
type recordingFetcher struct {
	mu   sync.Mutex
	hits []fetchHit
}

type fetchHit struct {
	actor string
	key   string
}

func (f *recordingFetcher) Fetch(ctx context.Context, key, etag string) (freshness.RefreshResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, fetchHit{actor: actor.FromContext(ctx), key: key})
	return freshness.RefreshResult{RecordsChanged: 1}, nil
}

func (f *recordingFetcher) keysForActor(act string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for _, h := range f.hits {
		if h.actor == act {
			keys = append(keys, h.key)
		}
	}
	return keys
}

func TestPeriodicRefresher_Start(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)
	t.Cleanup(func() { db.Close() })

	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)

	for _, kind := range []string{KindUser, KindUserOrgs, KindOrgRepos} {
		mgr.RegisterFetcher(freshness.Policy{Kind: kind}, &stubFetcher{})
	}

	// Seed some resources so the backstop has work to do.
	ctx := context.Background()
	require.NoError(t, mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: KindUser, Key: "self"}))

	// A single session in the default (empty) cache partition, matching the seed.
	sessions := func(ctx context.Context) ([]Session, error) {
		return []Session{{Ctx: ctx}}, nil
	}
	refresher := NewPeriodicRefresher(mgr, 50*time.Millisecond, sessions)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	refresher.Start(ctx)

	// If we get here without hanging, the context cancellation worked.
	assert.True(t, true)
}

// TestPeriodicRefresher_SeedsOrgRepos is the regression test for the webhook
// "no cached scope" bug: a session that covers an org must populate that org's
// repos into its (otherwise empty) partition, so the webhook dispatcher has an
// actor to apply to. Before the fix the refresher only re-fetched already-known
// resources, so a cold app-installation partition was never populated.
func TestPeriodicRefresher_SeedsOrgRepos(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)
	t.Cleanup(func() { db.Close() })

	mgr := freshness.NewManager(freshness.NewStore(db))

	orgRepos := &recordingFetcher{}
	mgr.RegisterFetcher(freshness.Policy{Kind: KindOrgRepos, DefaultTTL: time.Hour}, orgRepos)
	for _, kind := range []string{KindUser, KindUserOrgs, KindPRFiles, KindCompare} {
		mgr.RegisterFetcher(freshness.Policy{Kind: kind}, &stubFetcher{})
	}

	const installActor = "app-installation:123"
	sessions := func(ctx context.Context) ([]Session, error) {
		sctx := actor.WithActor(ctx, installActor)
		return []Session{{Ctx: sctx, Orgs: []string{"acme"}}}, nil
	}

	refresher := NewPeriodicRefresher(mgr, time.Hour, sessions)
	refresher.refreshAll(context.Background())

	// The org's repos were fetched in the installation's partition, even though
	// nothing had ever seeded that partition before.
	assert.Equal(t, []string{"acme"}, orgRepos.keysForActor(installActor))
}
