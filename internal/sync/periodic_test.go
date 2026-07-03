package sync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
)

func TestPeriodicRefresher_Start(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)
	t.Cleanup(func() { db.Close() })

	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)
	mgr.RegisterFetcher(freshness.Policy{Kind: KindOrgRepos}, &stubFetcher{})

	// Seed a resource so RefreshAllOfKind has work to do.
	ctx := context.Background()
	require.NoError(t, mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "org1"}))

	// A single session in the default (empty) cache partition, matching the
	// seed above.
	sessions := func(ctx context.Context) ([]context.Context, error) {
		return []context.Context{ctx}, nil
	}
	refresher := NewPeriodicRefresher(mgr, 50*time.Millisecond, sessions)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	refresher.Start(ctx)

	// If we get here without hanging, the context cancellation worked.
	assert.True(t, true)
}
