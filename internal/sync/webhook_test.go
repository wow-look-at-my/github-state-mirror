package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
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
			"number":     number,
			"node_id":    "PR_node",
			"title":      title,
			"html_url":   "https://github.com/" + owner + "/" + repo + "/pull/42",
			"draft":      false,
			"state":      state,
			"created_at": "2026-04-01T10:00:00Z",
			"updated_at": "2026-04-01T11:00:00Z",
			"additions":  5,
			"deletions":  2,
			"mergeable":  true,
			"user":       map[string]interface{}{"login": "alice", "type": "User", "avatar_url": "https://a.com/alice.png", "html_url": "https://github.com/alice"},
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

func TestDispatch_PullRequest_PayloadApplied(t *testing.T) {
	dispatcher, mgr, fStore, store := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindOrgRepos, "my-org")

	raw := makePRPayload(t, "opened", "open", "my-org", "my-repo", 42, "Add feature")
	event := webhook.ParseEvent("pull_request", raw)
	dispatcher.Dispatch(ctx, event)

	// OrgRepos should NOT be invalidated — payload was applied directly.
	assert.Equal(t, freshness.StateFresh, metaState(t, fStore, KindOrgRepos, "my-org"))

	// The PR is global truth now, REST-complete (webhook payloads carry the
	// REST-only fields).
	pr, err := store.GetPullRequest(ctx, "my-org", "my-repo", 42)
	require.Nil(t, err)
	assert.Equal(t, "Add feature", pr.Title)
	assert.Equal(t, "OPEN", pr.State)
	assert.Equal(t, sql.NullInt64{Int64: 5, Valid: true}, pr.Additions)
	assert.Equal(t, "alice", pr.AuthorLogin.String)
	assert.True(t, ghdata.PRRestComplete(pr), "webhook-fed rows are rest-complete")
	assert.NotEmpty(t, pr.TouchedAt, "webhook applies stamp touched_at")

	// The repos row was created from the payload's repository object.
	_, err = store.GetRepo(ctx, "my-org", "my-repo")
	require.NoError(t, err, "the payload's repository object seeds global truth")

	// Verify labels were written.
	labels, err := store.ListPRLabels(ctx, "my-org", "my-repo", 42)
	require.Nil(t, err)
	assert.Equal(t, 1, len(labels))
	assert.Equal(t, "enhancement", labels[0].Name)
}

func TestDispatch_PullRequest_ClosedDeletesPR(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	require.Nil(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "my-org", Repo: "my-repo", Number: 7, Title: "Old PR", Url: "https://github.com/my-org/my-repo/pull/7",
		State: "OPEN", CreatedAt: "2026-03-01T10:00:00Z", UpdatedAt: "2026-03-01T10:00:00Z",
	}, time.Now()))

	// Dispatch a "closed" webhook.
	raw := makePRPayload(t, "closed", "closed", "my-org", "my-repo", 7, "Old PR")
	event := webhook.ParseEvent("pull_request", raw)
	dispatcher.Dispatch(ctx, event)

	// The PR should be deleted from the DB.
	_, err := store.GetPullRequest(ctx, "my-org", "my-repo", 7)
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

// TestDispatch_PullRequest_SynchronizeResetsMergeable: a synchronize means the
// head moved and GitHub is recomputing mergeability -- the cached value must
// go unknown (so the /pulls/{n} gate misses) even though the payload's
// COALESCE-preserving upsert kept it.
func TestDispatch_PullRequest_SynchronizeResetsMergeable(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", makePRPayload(t, "opened", "open", "my-org", "my-repo", 42, "PR")))
	pr, err := store.GetPullRequest(ctx, "my-org", "my-repo", 42)
	require.NoError(t, err)
	require.True(t, pr.Mergeable.Valid, "the opened payload carried a resolved mergeable")

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", makePRPayload(t, "synchronize", "open", "my-org", "my-repo", 42, "PR")))
	pr, err = store.GetPullRequest(ctx, "my-org", "my-repo", 42)
	require.NoError(t, err)
	assert.False(t, pr.Mergeable.Valid, "synchronize must reset mergeable to unknown")
	assert.False(t, pr.MergeCommitSha.Valid, "synchronize must reset the test-merge sha")
}

func makeStatusPayload(t *testing.T, owner, repo, sha, state, context string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{
		"sha":     sha,
		"state":   state,
		"context": context,
		"repository": map[string]interface{}{
			"name":  repo,
			"owner": map[string]interface{}{"login": owner},
		},
	})
	require.Nil(t, err)
	return data
}

// TestDispatch_Status_AppliesRollup verifies a status webhook updates the PR's
// last_commit_status in place (no org invalidation, no re-fetch).
func TestDispatch_Status_AppliesRollup(t *testing.T) {
	dispatcher, mgr, fStore, store := setupDispatcher(t)
	ctx := context.Background()

	require.Nil(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "my-org", Repo: "my-repo", Number: 1, Title: "PR", Url: "u",
		State: "OPEN", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		HeadRefOid: sql.NullString{String: "sha1", Valid: true},
	}, time.Now()))
	seed(t, mgr, KindOrgRepos, "my-org")

	dispatcher.Dispatch(ctx, webhook.ParseEvent("status", makeStatusPayload(t, "my-org", "my-repo", "sha1", "success", "ci/build")))

	pr, err := store.GetPullRequest(ctx, "my-org", "my-repo", 1)
	require.Nil(t, err)
	assert.Equal(t, "SUCCESS", pr.LastCommitStatus.String)

	// org repos must NOT be invalidated — the rollup was applied directly.
	assert.Equal(t, freshness.StateFresh, metaState(t, fStore, KindOrgRepos, "my-org"))

	// A second, failing context flips the rollup to FAILURE.
	dispatcher.Dispatch(ctx, webhook.ParseEvent("status", makeStatusPayload(t, "my-org", "my-repo", "sha1", "failure", "ci/test")))
	pr, err = store.GetPullRequest(ctx, "my-org", "my-repo", 1)
	require.Nil(t, err)
	assert.Equal(t, "FAILURE", pr.LastCommitStatus.String)
}

// TestDispatch_AppliedRecordsDelivery verifies the dispatch returns an "applied"
// result and records it (with the delivery id) in the global webhook log.
func TestDispatch_AppliedRecordsDelivery(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	event := webhook.ParseEvent("pull_request", makePRPayload(t, "opened", "open", "my-org", "my-repo", 42, "Add feature"))
	event.DeliveryID = "delivery-42"

	result := dispatcher.Dispatch(ctx, event)

	assert.Equal(t, webhook.DispApplied, result.Disposition)
	assert.Equal(t, "my-org/my-repo", result.Repo)
	assert.Equal(t, http.StatusOK, result.StatusCode())

	deliveries, err := store.RecentWebhookDeliveries(ctx, 10)
	require.Nil(t, err)
	require.Len(t, deliveries, 1)
	assert.Equal(t, "delivery-42", deliveries[0].DeliveryID)
	assert.Equal(t, "pull_request", deliveries[0].EventType)
	assert.Equal(t, "opened", deliveries[0].Action)
	assert.Equal(t, webhook.DispApplied, deliveries[0].Disposition)
}

// TestDispatch_IgnoredUntrackedEvent verifies an event the mirror does not track
// (e.g. deployment_status) is recorded as "ignored" rather than dropped invisibly.
func TestDispatch_IgnoredUntrackedEvent(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	event := webhook.Event{Type: "deployment_status", Action: "created"}
	result := dispatcher.Dispatch(ctx, event)

	assert.Equal(t, webhook.DispIgnored, result.Disposition)
	assert.Equal(t, http.StatusAccepted, result.StatusCode())

	deliveries, err := store.RecentWebhookDeliveries(ctx, 10)
	require.Nil(t, err)
	require.Len(t, deliveries, 1)
	assert.Equal(t, "deployment_status", deliveries[0].EventType)
	assert.Equal(t, webhook.DispIgnored, deliveries[0].Disposition)
}

// TestDispatch_PRUpsert_PreservesStatus verifies a later pull_request webhook
// (which carries no CI status) doesn't wipe a status set by a check webhook.
func TestDispatch_PRUpsert_PreservesStatus(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	require.Nil(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "my-org", Repo: "my-repo", Number: 42, Title: "PR", Url: "u",
		State: "OPEN", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		HeadRefOid: sql.NullString{String: "abc123", Valid: true},
	}, time.Now()))

	// CI status arrives first.
	dispatcher.Dispatch(ctx, webhook.ParseEvent("status", makeStatusPayload(t, "my-org", "my-repo", "abc123", "success", "ci")))

	// Then a pull_request webhook (e.g. "labeled") with no CI status.
	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", makePRPayload(t, "labeled", "open", "my-org", "my-repo", 42, "PR")))

	pr, err := store.GetPullRequest(ctx, "my-org", "my-repo", 42)
	require.Nil(t, err)
	assert.Equal(t, "SUCCESS", pr.LastCommitStatus.String, "PR upsert must not clobber CI status")
}

func makePushPayload(t *testing.T, owner, repo, ref, ts string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{
		"ref":         ref,
		"repository":  map[string]interface{}{"name": repo, "owner": map[string]interface{}{"login": owner}},
		"head_commit": map[string]interface{}{"timestamp": ts},
	})
	require.Nil(t, err)
	return data
}

func TestDispatch_Push_UpdatesPushedAt(t *testing.T) {
	dispatcher, mgr, fStore, store := setupDispatcher(t)
	ctx := context.Background()

	require.Nil(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "u",
		PushedAt: sql.NullString{String: "2020-01-01T00:00:00Z", Valid: true},
	}))
	seed(t, mgr, KindOrgRepos, "my-org")

	dispatcher.Dispatch(ctx, webhook.ParseEvent("push", makePushPayload(t, "my-org", "my-repo", "refs/heads/main", "2026-05-01T12:00:00Z")))

	repo, err := store.GetRepo(ctx, "my-org", "my-repo")
	require.Nil(t, err)
	assert.Equal(t, "2026-05-01T12:00:00Z", repo.PushedAt.String)

	assert.Equal(t, freshness.StateFresh, metaState(t, fStore, KindOrgRepos, "my-org"))
}

// TestDispatch_Push_UnresolvesMergeableByBranch: a push to a branch un-resolves
// mergeable for every open PR based on (or heading from) it -- GitHub is
// recomputing and never webhooks the result.
func TestDispatch_Push_UnresolvesMergeableByBranch(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	now := time.Now()

	mk := func(n int64, baseRef, headRef string) dbgen.PullRequest {
		return dbgen.PullRequest{
			Owner: "my-org", Repo: "my-repo", Number: n, Title: "PR", Url: "u",
			State: "OPEN", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
			BaseRefName: sql.NullString{String: baseRef, Valid: true},
			HeadRefName: sql.NullString{String: headRef, Valid: true},
			Mergeable:   sql.NullString{String: "MERGEABLE", Valid: true},
		}
	}
	require.NoError(t, store.UpsertPR(ctx, mk(1, "main", "feature-a"), now))    // based on main
	require.NoError(t, store.UpsertPR(ctx, mk(2, "develop", "main"), now))      // heads from main
	require.NoError(t, store.UpsertPR(ctx, mk(3, "develop", "feature-b"), now)) // unrelated

	dispatcher.Dispatch(ctx, webhook.ParseEvent("push", makePushPayload(t, "my-org", "my-repo", "refs/heads/main", "2026-05-01T12:00:00Z")))

	pr1, _ := store.GetPullRequest(ctx, "my-org", "my-repo", 1)
	pr2, _ := store.GetPullRequest(ctx, "my-org", "my-repo", 2)
	pr3, _ := store.GetPullRequest(ctx, "my-org", "my-repo", 3)
	assert.False(t, pr1.Mergeable.Valid, "base push must un-resolve")
	assert.False(t, pr2.Mergeable.Valid, "head push must un-resolve")
	assert.True(t, pr3.Mergeable.Valid, "unrelated PRs keep their answer")
}

func TestDispatch_PullRequestReview_AppliesPR(t *testing.T) {
	dispatcher, mgr, fStore, store := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindOrgRepos, "my-org")

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request_review",
		makePRPayload(t, "submitted", "open", "my-org", "my-repo", 5, "Reviewed PR")))

	pr, err := store.GetPullRequest(ctx, "my-org", "my-repo", 5)
	require.Nil(t, err)
	assert.Equal(t, "Reviewed PR", pr.Title)

	assert.Equal(t, freshness.StateFresh, metaState(t, fStore, KindOrgRepos, "my-org"))
}

func makeLabelPayload(t *testing.T, action, owner, repo, name, color string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{
		"action":     action,
		"label":      map[string]interface{}{"name": name, "color": color},
		"repository": map[string]interface{}{"name": repo, "owner": map[string]interface{}{"login": owner}},
	})
	require.Nil(t, err)
	return data
}

func TestDispatch_Label_RecolorAndDelete(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	require.Nil(t, store.SetPRLabels(ctx, "my-org", "my-repo", 1, []dbgen.PrLabel{
		{Owner: "my-org", Repo: "my-repo", PrNumber: 1, Name: "bug", Color: "aaaaaa"},
	}))

	dispatcher.Dispatch(ctx, webhook.ParseEvent("label", makeLabelPayload(t, "edited", "my-org", "my-repo", "bug", "bbbbbb")))
	labels, err := store.ListPRLabels(ctx, "my-org", "my-repo", 1)
	require.Nil(t, err)
	require.Equal(t, 1, len(labels))
	assert.Equal(t, "bbbbbb", labels[0].Color)

	dispatcher.Dispatch(ctx, webhook.ParseEvent("label", makeLabelPayload(t, "deleted", "my-org", "my-repo", "bug", "bbbbbb")))
	labels, err = store.ListPRLabels(ctx, "my-org", "my-repo", 1)
	require.Nil(t, err)
	assert.Equal(t, 0, len(labels))
}

func makeCheckSuitePayload(t *testing.T, owner, repo, sha, headBranch, defaultBranch, conclusion string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{
		"check_suite": map[string]interface{}{
			"head_sha":    sha,
			"head_branch": headBranch,
			"status":      "completed",
			"conclusion":  conclusion,
			"app":         map[string]interface{}{"slug": "actions"},
		},
		"repository": map[string]interface{}{
			"name":           repo,
			"default_branch": defaultBranch,
			"owner":          map[string]interface{}{"login": owner},
		},
	})
	require.Nil(t, err)
	return data
}

// TestDispatch_CheckSuite_DefaultBranchStatus verifies a check_suite on the
// default branch updates the repo's default_branch_status in place, and one on
// another branch does not.
func TestDispatch_CheckSuite_DefaultBranchStatus(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	require.Nil(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "u",
		DefaultBranch: sql.NullString{String: "main", Valid: true},
	}))

	dispatcher.Dispatch(ctx, webhook.ParseEvent("check_suite",
		makeCheckSuitePayload(t, "my-org", "my-repo", "sha9", "main", "main", "success")))
	repo, err := store.GetRepo(ctx, "my-org", "my-repo")
	require.Nil(t, err)
	assert.Equal(t, "SUCCESS", repo.DefaultBranchStatus.String)

	dispatcher.Dispatch(ctx, webhook.ParseEvent("check_suite",
		makeCheckSuitePayload(t, "my-org", "my-repo", "sha10", "feature", "main", "failure")))
	repo, err = store.GetRepo(ctx, "my-org", "my-repo")
	require.Nil(t, err)
	assert.Equal(t, "SUCCESS", repo.DefaultBranchStatus.String, "non-default branch must not change default_branch_status")
}

// TestDispatch_RepositoryLifecycle covers the direct repository-event applies:
// visibility flips, deletion (cascade), and renames.
func TestDispatch_RepositoryLifecycle(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	repoObj := func(name, visibility string, private bool) map[string]interface{} {
		return map[string]interface{}{
			"name": name, "full_name": "my-org/" + name,
			"private": private, "visibility": visibility,
			"html_url": "https://github.com/my-org/" + name, "default_branch": "main",
			"owner": map[string]interface{}{"login": "my-org"},
		}
	}
	mkEvent := func(action string, repo map[string]interface{}, extra map[string]interface{}) webhook.Event {
		payload := map[string]interface{}{"action": action, "repository": repo}
		for k, v := range extra {
			payload[k] = v
		}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		return webhook.ParseEvent("repository", raw)
	}

	// created: absorbed with visibility.
	result := dispatcher.Dispatch(ctx, mkEvent("created", repoObj("r1", "public", false), nil))
	assert.Equal(t, webhook.DispApplied, result.Disposition)
	repo, err := store.GetRepo(ctx, "my-org", "r1")
	require.NoError(t, err)
	assert.Equal(t, ghdata.VisibilityPublic, repo.Visibility)

	// privatized: the public fast path must close.
	result = dispatcher.Dispatch(ctx, mkEvent("privatized", repoObj("r1", "private", true), nil))
	assert.Equal(t, webhook.DispApplied, result.Disposition)
	repo, _ = store.GetRepo(ctx, "my-org", "r1")
	assert.Equal(t, ghdata.VisibilityPrivate, repo.Visibility)

	// publicized: it reopens.
	dispatcher.Dispatch(ctx, mkEvent("publicized", repoObj("r1", "public", false), nil))
	repo, _ = store.GetRepo(ctx, "my-org", "r1")
	assert.Equal(t, ghdata.VisibilityPublic, repo.Visibility)

	// renamed: the old row's truth is dropped, the new one stands.
	dispatcher.Dispatch(ctx, mkEvent("renamed", repoObj("r1-new", "public", false),
		map[string]interface{}{"changes": map[string]interface{}{"repository": map[string]interface{}{"name": map[string]interface{}{"from": "r1"}}}}))
	_, err = store.GetRepo(ctx, "my-org", "r1")
	assert.ErrorIs(t, err, sql.ErrNoRows, "the renamed-away row is removed")
	_, err = store.GetRepo(ctx, "my-org", "r1-new")
	assert.NoError(t, err)

	// deleted: the row and its dependents cascade away.
	result = dispatcher.Dispatch(ctx, mkEvent("deleted", repoObj("r1-new", "public", false), nil))
	assert.Equal(t, webhook.DispApplied, result.Disposition)
	_, err = store.GetRepo(ctx, "my-org", "r1-new")
	assert.ErrorIs(t, err, sql.ErrNoRows)
}
