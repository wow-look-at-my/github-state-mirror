package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

type stubFetcher struct{}

func (f *stubFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	return freshness.RefreshResult{RecordsChanged: 1}, nil
}

func setupDispatcher(t *testing.T) (*WebhookDispatcher, *freshness.Manager, *freshness.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)
	t.Cleanup(func() { db.Close() })

	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)

	// Register stub fetchers so invalidate can find metadata.
	for _, kind := range []string{KindUser, KindUserOrgs, KindOrgRepos, KindPRFiles, KindCompare} {
		mgr.RegisterFetcher(freshness.Policy{Kind: kind}, &stubFetcher{})
	}

	dispatcher := NewWebhookDispatcher(mgr)
	return dispatcher, mgr, fStore
}

// seed creates a fresh metadata entry so Invalidate has something to mark stale.
func seed(t *testing.T, mgr *freshness.Manager, kind, key string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: kind, Key: key}))
}

func TestDispatch_Push(t *testing.T) {
	dispatcher, mgr, fStore := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindOrgRepos, "my-org")

	event := webhook.Event{
		Type:           "push",
		RepoOwnerLogin: "my-org",
		RepoNameStr:    "my-repo",
	}
	dispatcher.Dispatch(ctx, event)

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)
}

func TestDispatch_PullRequest(t *testing.T) {
	dispatcher, mgr, fStore := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindOrgRepos, "my-org")
	seed(t, mgr, KindPRFiles, "my-org/my-repo/42")
	seed(t, mgr, KindCompare, "my-org/my-repo/main...feature")

	event := webhook.Event{
		Type:           "pull_request",
		Action:         "opened",
		RepoOwnerLogin: "my-org",
		RepoNameStr:    "my-repo",
		PRNumber:       42,
		PRBase:         "main",
		PRHead:         "feature",
	}
	dispatcher.Dispatch(ctx, event)

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)

	meta, err = fStore.Get(ctx, freshness.ResourceID{Kind: KindPRFiles, Key: "my-org/my-repo/42"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)

	meta, err = fStore.Get(ctx, freshness.ResourceID{Kind: KindCompare, Key: "my-org/my-repo/main...feature"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)
}

func TestDispatch_PullRequestReview(t *testing.T) {
	dispatcher, mgr, fStore := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindOrgRepos, "my-org")

	event := webhook.Event{
		Type:           "pull_request_review",
		Action:         "submitted",
		RepoOwnerLogin: "my-org",
		RepoNameStr:    "my-repo",
	}
	dispatcher.Dispatch(ctx, event)

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)
}

func TestDispatch_CheckRun(t *testing.T) {
	dispatcher, mgr, fStore := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindOrgRepos, "my-org")

	for _, eventType := range []string{"check_run", "check_suite", "status"} {
		event := webhook.Event{
			Type:           eventType,
			RepoOwnerLogin: "my-org",
			RepoNameStr:    "my-repo",
		}
		dispatcher.Dispatch(ctx, event)
	}

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)
}

func TestDispatch_Repository(t *testing.T) {
	dispatcher, mgr, fStore := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindOrgRepos, "my-org")

	event := webhook.Event{
		Type:           "repository",
		Action:         "created",
		RepoOwnerLogin: "my-org",
		RepoNameStr:    "new-repo",
	}
	dispatcher.Dispatch(ctx, event)

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)
}

func TestDispatch_Organization(t *testing.T) {
	dispatcher, mgr, fStore := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindUserOrgs, "my-org")

	event := webhook.Event{
		Type:     "organization",
		Action:   "member_added",
		OrgLogin: "my-org",
	}
	dispatcher.Dispatch(ctx, event)

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindUserOrgs, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)
}

func TestDispatch_Membership(t *testing.T) {
	dispatcher, mgr, fStore := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindUserOrgs, "my-org")

	event := webhook.Event{
		Type:     "membership",
		Action:   "added",
		OrgLogin: "my-org",
	}
	dispatcher.Dispatch(ctx, event)

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindUserOrgs, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)
}

func TestDispatch_Label(t *testing.T) {
	dispatcher, mgr, fStore := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindOrgRepos, "my-org")

	event := webhook.Event{
		Type:           "label",
		Action:         "created",
		RepoOwnerLogin: "my-org",
		RepoNameStr:    "my-repo",
	}
	dispatcher.Dispatch(ctx, event)

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)
}

func TestDispatch_UnknownEvent(t *testing.T) {
	dispatcher, _, _ := setupDispatcher(t)
	ctx := context.Background()

	// Should not panic on unknown event types.
	event := webhook.Event{
		Type: "unknown_event",
	}
	dispatcher.Dispatch(ctx, event)
}

func TestDispatch_PullRequest_NoBranches(t *testing.T) {
	dispatcher, mgr, fStore := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindOrgRepos, "my-org")
	seed(t, mgr, KindPRFiles, "my-org/my-repo/10")

	// PR event without branch info — should not invalidate compare.
	event := webhook.Event{
		Type:           "pull_request",
		Action:         "labeled",
		RepoOwnerLogin: "my-org",
		RepoNameStr:    "my-repo",
		PRNumber:       10,
	}
	dispatcher.Dispatch(ctx, event)

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindPRFiles, Key: "my-org/my-repo/10"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateStale, meta.State)
}
