package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

type stubFetcher struct{}

func (f *stubFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	return freshness.RefreshResult{RecordsChanged: 1}, nil
}

func setupDispatcher(t *testing.T) (*WebhookDispatcher, *freshness.Manager, *freshness.Store, *ghdata.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)
	t.Cleanup(func() { db.Close() })

	fStore := freshness.NewStore(db)
	mgr := freshness.NewManager(fStore)
	store := ghdata.NewStore(db)

	// Register a stub fetcher so invalidate can find metadata.
	mgr.RegisterFetcher(freshness.Policy{Kind: KindOrgRepos}, &stubFetcher{})

	dispatcher := NewWebhookDispatcher(mgr, store)
	return dispatcher, mgr, fStore, store
}

// TestDispatch_NeverSeenRepoAppliesGlobally is the heart of the global model:
// a stateful delivery for a repo NOBODY has ever fetched applies straight to
// global truth (the repos row is created from the payload's own repository
// object) -- there is no "no cached scope" skip, no on-demand pull, no app
// needed. (Operator directive: "just because nobody has fetched something
// doesn't mean we get to ignore updates from webhooks for it.")
func TestDispatch_NeverSeenRepoAppliesGlobally(t *testing.T) {
	d, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	raw := []byte(`{
		"sha": "deadbeef",
		"state": "success",
		"context": "ci/test",
		"branches": [{"name": "main"}],
		"repository": {"name": "repo-nightmare", "full_name": "wow-look-at-my/repo-nightmare",
			"private": true, "visibility": "private", "default_branch": "main",
			"html_url": "https://github.com/wow-look-at-my/repo-nightmare",
			"owner": {"login": "wow-look-at-my"}},
		"installation": {"id": 123}
	}`)
	result := d.Dispatch(ctx, webhook.ParseEvent("status", raw))

	assert.Equal(t, webhook.DispApplied, result.Disposition, "a never-seen repo's event must apply, never skip")
	assert.Equal(t, http.StatusOK, result.StatusCode())

	// The repos row was absorbed from the payload, visibility and all.
	repo, err := store.GetRepo(ctx, "wow-look-at-my", "repo-nightmare")
	require.NoError(t, err)
	assert.Equal(t, ghdata.VisibilityPrivate, repo.Visibility)
	assert.Equal(t, "main", repo.DefaultBranch.String)
	// And the check state landed in global truth (readable once revealed).
	assert.Equal(t, "SUCCESS", repo.DefaultBranchStatus.String, "the default-branch status applied on first contact")
}

// seed creates a fresh metadata entry so Invalidate has something to mark stale.
func seed(t *testing.T, mgr *freshness.Manager, kind, key string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: kind, Key: key, Actor: "user:1"}))
}

func metaState(t *testing.T, fStore *freshness.Store, kind, key string) freshness.FetchState {
	t.Helper()
	meta, err := fStore.Get(context.Background(), freshness.ResourceID{Kind: kind, Key: key, Actor: "user:1"})
	require.Nil(t, err)
	require.NotNil(t, meta)
	return meta.State
}

// Unparseable payloads fall back to marking every principal's org sync stale.
func TestDispatch_UnparseablePayloadsInvalidate(t *testing.T) {
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
	ctx := context.Background()

	for _, eventType := range []string{"push", "pull_request", "pull_request_review", "check_run", "check_suite", "status"} {
		seed(t, mgr, KindOrgRepos, "my-org")
		event := webhook.Event{
			Type:           eventType,
			RepoOwnerLogin: "my-org",
			RepoNameStr:    "my-repo",
		}
		result := dispatcher.Dispatch(ctx, event)
		assert.Equal(t, webhook.DispInvalidated, result.Disposition, eventType)
		assert.Equal(t, freshness.StateStale, metaState(t, fStore, KindOrgRepos, "my-org"), eventType)
	}
}

// Org/membership events change WHO can see what: every principal's org sync
// marker goes stale so their next read re-syncs their grant set.
func TestDispatch_OrgChangesInvalidateSyncMarkers(t *testing.T) {
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
	ctx := context.Background()

	for _, eventType := range []string{"organization", "membership"} {
		seed(t, mgr, KindOrgRepos, "my-org")
		event := webhook.Event{Type: eventType, Action: "member_added", OrgLogin: "my-org"}
		result := dispatcher.Dispatch(ctx, event)
		assert.Equal(t, webhook.DispInvalidated, result.Disposition, eventType)
		assert.Equal(t, freshness.StateStale, metaState(t, fStore, KindOrgRepos, "my-org"), eventType)
	}
}

func TestDispatch_UnknownEvent(t *testing.T) {
	dispatcher, _, _, _ := setupDispatcher(t)
	ctx := context.Background()

	// Should not panic on unknown event types.
	event := webhook.Event{
		Type: "unknown_event",
	}
	dispatcher.Dispatch(ctx, event)
}

// TestDispatch_PushAndRepositoryFlushCommitsListSnapshots: push and repository
// events flush a repo's cached commits-list snapshots (a push moves every
// ref-relative listing) while the absorbed git-commit rows -- immutable global
// truth -- survive the flush.
func TestDispatch_PushAndRepositoryFlushCommitsListSnapshots(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	now := time.Now()

	commit := ghdata.CachedGitCommit{
		Owner: "org1", Repo: "repo1",
		SHA:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TreeSHA: "1111111111111111111111111111111111111111",
		Message: "m",
	}
	seedSnapshot := func() {
		t.Helper()
		require.NoError(t, store.PutCachedCommitsList(ctx, "org1", "repo1", "main", 100, 1,
			[]ghdata.CachedGitCommit{commit}, now, time.Hour))
		_, ok, err := store.GetCachedCommitsList(ctx, "org1", "repo1", "main", 100, 1, now)
		require.NoError(t, err)
		require.True(t, ok, "seeded snapshot must serve")
	}
	snapshotServes := func() bool {
		t.Helper()
		_, ok, err := store.GetCachedCommitsList(ctx, "org1", "repo1", "main", 100, 1, now)
		require.NoError(t, err)
		return ok
	}

	seedSnapshot()
	dispatcher.Dispatch(ctx, webhook.ParseEvent("push",
		[]byte(`{"repository":{"name":"repo1","full_name":"org1/repo1","owner":{"login":"org1"}}}`)))
	assert.False(t, snapshotServes(), "a push must flush the repo's commits-list snapshots")

	seedSnapshot()
	dispatcher.Dispatch(ctx, webhook.ParseEvent("repository",
		[]byte(`{"action":"privatized","repository":{"name":"repo1","full_name":"org1/repo1","owner":{"login":"org1"}}}`)))
	assert.False(t, snapshotServes(), "a repository event must flush the repo's commits-list snapshots")

	// The absorbed commit rows are immutable and survive both flushes.
	_, ok, err := store.GetCachedGitCommit(ctx, "org1", "repo1", commit.SHA, now)
	require.NoError(t, err)
	assert.True(t, ok, "git-commit rows must survive the snapshot flush")
}

// TestDispatch_PushAndRepositoryFlushCompareCache: push and repository events
// flush a repo's cached compare docs (a push to either side of any basehead
// changes the comparison) while the absorbed git-commit rows -- immutable
// global truth -- survive the flush.
func TestDispatch_PushAndRepositoryFlushCompareCache(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	now := time.Now()

	commit := ghdata.CachedGitCommit{
		Owner: "org1", Repo: "repo1",
		SHA:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TreeSHA: "1111111111111111111111111111111111111111",
		Message: "m",
	}
	const basehead = "main...claude/dev"
	seedCompare := func() {
		t.Helper()
		require.NoError(t, store.PutCachedCompare(ctx, ghdata.CachedCompare{
			Owner: "org1", Repo: "repo1", Basehead: basehead,
			BaseRef: "main", HeadRef: "claude/dev", Status: 200,
			Doc: `{"status":"ahead","ahead_by":1,"behind_by":0,"total_commits":1,"commits":[],"files":[]}`,
		}, []ghdata.CachedGitCommit{commit}, now, time.Hour))
		_, ok, err := store.GetCachedCompare(ctx, "org1", "repo1", basehead, now)
		require.NoError(t, err)
		require.True(t, ok, "seeded compare doc must serve")
	}
	compareServes := func() bool {
		t.Helper()
		_, ok, err := store.GetCachedCompare(ctx, "org1", "repo1", basehead, now)
		require.NoError(t, err)
		return ok
	}

	seedCompare()
	dispatcher.Dispatch(ctx, webhook.ParseEvent("push",
		[]byte(`{"repository":{"name":"repo1","full_name":"org1/repo1","owner":{"login":"org1"}}}`)))
	assert.False(t, compareServes(), "a push must flush the repo's compare docs")

	seedCompare()
	dispatcher.Dispatch(ctx, webhook.ParseEvent("repository",
		[]byte(`{"action":"privatized","repository":{"name":"repo1","full_name":"org1/repo1","owner":{"login":"org1"}}}`)))
	assert.False(t, compareServes(), "a repository event must flush the repo's compare docs")

	// The absorbed commit rows are immutable and survive both flushes.
	_, ok, err := store.GetCachedGitCommit(ctx, "org1", "repo1", commit.SHA, now)
	require.NoError(t, err)
	assert.True(t, ok, "git-commit rows must survive the compare flush")
}

// makePRPayload builds a realistic pull_request webhook JSON payload.
func makePRPayload(t *testing.T, action, state, owner, repo string, number int, title string) json.RawMessage {
	t.Helper()
	payload := map[string]interface{}{
		"action": action,
		"repository": map[string]interface{}{
			"name":  repo,
			"owner": map[string]interface{}{"login": owner},
		},
		"pull_request": map[string]interface{}{
			"number":          number,
			"node_id":         "PR_node",
			"title":           title,
			"html_url":        "https://github.com/" + owner + "/" + repo + "/pull/42",
			"draft":           false,
			"state":           state,
			"created_at":      "2026-04-01T10:00:00Z",
			"updated_at":      "2026-04-01T11:00:00Z",
			"additions":       5,
			"deletions":       2,
			"mergeable":       true,
			"mergeable_state": "clean",
			"user":            map[string]interface{}{"login": "alice", "type": "User", "avatar_url": "https://a.com/alice.png", "html_url": "https://github.com/alice"},
			"head": map[string]interface{}{
				"ref": "feature", "sha": "abc123",
				"repo": map[string]interface{}{"full_name": owner + "/" + repo},
			},
			"base": map[string]interface{}{
				"ref": "main", "sha": "base456",
				"repo": map[string]interface{}{
					"name":  repo,
					"owner": map[string]interface{}{"login": owner},
				},
			},
			"labels":              []map[string]interface{}{{"name": "enhancement", "color": "a2eeef"}},
			"requested_reviewers": []interface{}{},
			"requested_teams":     []interface{}{},
		},
	}
	data, err := json.Marshal(payload)
	require.Nil(t, err)
	return data
}
