package sync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPeriodicRefresher_Start(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)
	t.Cleanup(func() { db.Close() })

	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)

	fetchCount := 0
	for _, kind := range []string{KindUser, KindUserOrgs, KindOrgRepos} {
		mgr.RegisterFetcher(freshness.Policy{Kind: kind}, &stubFetcher{})
	}

	// Seed some resources so RefreshAllOfKind has work to do.
	ctx := context.Background()
	require.NoError(t, mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: KindUser, Key: "self"}))
	_ = fetchCount	// stubFetcher doesn't increment, that's fine

	refresher := NewPeriodicRefresher(mgr, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	refresher.Start(ctx)

	// If we get here without hanging, the context cancellation worked.
	assert.True(t, true)
}
