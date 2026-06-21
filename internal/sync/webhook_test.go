package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

type stubFetcher struct{}

func (f *stubFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	return freshness.RefreshResult{RecordsChanged: 1}, nil
}

// repoSeedingFetcher stands in for the org-repos fetcher: it writes a single
// repo row into whatever partition asked for it (read from the context actor),
// so an on-demand pull establishes a cache scope without a real GitHub call.
type repoSeedingFetcher struct {
	store       *ghdata.Store
	owner, repo string
}

func (f *repoSeedingFetcher) Fetch(ctx context.Context, key, etag string) (freshness.RefreshResult, error) {
	err := f.store.UpsertRepo(ctx, dbgen.Repo{
		Owner:         f.owner,
		Name:          f.repo,
		NameWithOwner: f.owner + "/" + f.repo,
	})
	return freshness.RefreshResult{RecordsChanged: 1}, err
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

	// Register stub fetchers so invalidate can find metadata.
	for _, kind := range []string{KindUser, KindUserOrgs, KindOrgRepos, KindPRFiles, KindCompare} {
		mgr.RegisterFetcher(freshness.Policy{Kind: kind}, &stubFetcher{})
	}

	dispatcher := NewWebhookDispatcher(mgr, store, nil)
	return dispatcher, mgr, fStore, store
}

// TestDispatch_PullsUncachedRepoOnDemand is the regression test for the webhook
// "no cached scope" bug. A status delivery arrives for a repo no partition has
// cached; rather than skip, the dispatcher pulls the repo on demand (as the
// installation named in the delivery) and then applies the status.
func TestDispatch_PullsUncachedRepoOnDemand(t *testing.T) {
	_, mgr, _, store := setupDispatcher(t)

	const owner, repo = "wow-look-at-my", "repo-nightmare"
	const installID int64 = 123

	// Fake GitHub: the dispatcher only needs an installation token; the org-repos
	// fetch itself is stood in for by repoSeedingFetcher below.
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/123/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_inst123"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	gh := ghclient.NewWithBaseURL(srv.URL)
	app, err := ghclient.NewAppAuthenticator("42", testAppKeyPEM(t), gh)
	require.NoError(t, err)

	mgr.RegisterFetcher(freshness.Policy{Kind: KindOrgRepos}, &repoSeedingFetcher{store: store, owner: owner, repo: repo})

	d := NewWebhookDispatcher(mgr, store, app)

	raw := []byte(`{
		"sha": "deadbeef",
		"state": "success",
		"context": "ci/test",
		"branches": [{"name": "main"}],
		"repository": {"name": "repo-nightmare", "default_branch": "main", "owner": {"login": "wow-look-at-my"}},
		"installation": {"id": 123}
	}`)
	event := webhook.ParseEvent("status", raw)
	require.Equal(t, installID, event.InstallationID)

	result := d.Dispatch(context.Background(), event)

	// Applied, not skipped: the uncached repo was pulled first, then the status
	// rolled up onto the (now cached) installation partition.
	assert.Equal(t, webhook.DispApplied, result.Disposition)
	assert.Equal(t, 1, result.Scopes)

	// The pulled repo lives in the installation's partition.
	actors, err := store.ActorsForRepo(context.Background(), owner, repo)
	require.NoError(t, err)
	assert.Equal(t, []string{AppInstallationActor(installID)}, actors)
}

// TestDispatch_SkipsWhenNoAppToPull confirms the pull is best-effort: with no
// app configured the uncached repo cannot be fetched, so the delivery still
// skips cleanly rather than erroring.
func TestDispatch_SkipsWhenNoAppToPull(t *testing.T) {
	d, _, _, _ := setupDispatcher(t) // nil app

	raw := []byte(`{
		"sha": "deadbeef",
		"state": "success",
		"context": "ci/test",
		"repository": {"name": "repo-nightmare", "owner": {"login": "wow-look-at-my"}},
		"installation": {"id": 123}
	}`)
	result := d.Dispatch(context.Background(), webhook.ParseEvent("status", raw))

	assert.Equal(t, webhook.DispSkipped, result.Disposition)
	assert.Equal(t, 0, result.Scopes)
}

// seed creates a fresh metadata entry so Invalidate has something to mark stale.
func seed(t *testing.T, mgr *freshness.Manager, kind, key string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: kind, Key: key}))
}

func TestDispatch_Push(t *testing.T) {
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
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
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
	ctx := context.Background()

	seed(t, mgr, KindOrgRepos, "my-org")

	// This event carries no parseable Raw payload, so the dispatcher falls back to
	// invalidating the org-repos cache. (PR file lists and branch comparisons are
	// no longer cached, so there is nothing content-dependent to invalidate.)
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
}

func TestDispatch_PullRequestReview(t *testing.T) {
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
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
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
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
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
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
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
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
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
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
	dispatcher, mgr, fStore, _ := setupDispatcher(t)
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
			"title":      title,
			"html_url":   "https://github.com/" + owner + "/" + repo + "/pull/42",
			"draft":      false,
			"state":      state,
			"created_at": "2026-04-01T10:00:00Z",
			"updated_at": "2026-04-01T11:00:00Z",
			"additions":  5,
			"deletions":  2,
			"mergeable":  true,
			"user":       map[string]interface{}{"login": "alice", "avatar_url": "https://a.com/alice.png", "html_url": "https://github.com/alice"},
			"head":       map[string]interface{}{"ref": "feature", "sha": "abc123"},
			"base": map[string]interface{}{
				"ref": "main",
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

	// Seed a repo in the DB so ActorsForRepo finds the actor.
	actorCtx := actor.WithActor(ctx, "test-user")
	require.Nil(t, store.UpsertRepo(actorCtx, dbgen.Repo{
		Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "https://github.com/my-org/my-repo",
	}))

	seed(t, mgr, KindOrgRepos, "my-org")

	raw := makePRPayload(t, "opened", "open", "my-org", "my-repo", 42, "Add feature")
	event := webhook.ParseEvent("pull_request", raw)
	dispatcher.Dispatch(ctx, event)

	// OrgRepos should NOT be invalidated — payload was applied directly.
	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateFresh, meta.State)

	// Verify the PR was written to the DB.
	pr, err := store.GetPullRequest(actorCtx, "my-org", "my-repo", 42)
	require.Nil(t, err)
	assert.Equal(t, "Add feature", pr.Title)
	assert.Equal(t, "OPEN", pr.State)
	assert.Equal(t, sql.NullInt64{Int64: 5, Valid: true}, pr.Additions)
	assert.Equal(t, "alice", pr.AuthorLogin.String)

	// Verify labels were written.
	labels, err := store.ListPRLabels(actorCtx, "my-org", "my-repo", 42)
	require.Nil(t, err)
	assert.Equal(t, 1, len(labels))
	assert.Equal(t, "enhancement", labels[0].Name)
}

func TestDispatch_PullRequest_ClosedDeletesPR(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	// Seed a repo and an existing open PR in the DB.
	actorCtx := actor.WithActor(ctx, "test-user")
	require.Nil(t, store.UpsertRepo(actorCtx, dbgen.Repo{
		Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "https://github.com/my-org/my-repo",
	}))
	require.Nil(t, store.UpsertPR(actorCtx, dbgen.PullRequest{
		Owner: "my-org", Repo: "my-repo", Number: 7, Title: "Old PR", Url: "https://github.com/my-org/my-repo/pull/7",
		State: "OPEN", CreatedAt: "2026-03-01T10:00:00Z", UpdatedAt: "2026-03-01T10:00:00Z",
	}))

	// Dispatch a "closed" webhook.
	raw := makePRPayload(t, "closed", "closed", "my-org", "my-repo", 7, "Old PR")
	event := webhook.ParseEvent("pull_request", raw)
	dispatcher.Dispatch(ctx, event)

	// The PR should be deleted from the DB.
	_, err := store.GetPullRequest(actorCtx, "my-org", "my-repo", 7)
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestDispatch_PullRequest_MultipleActors(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	// Seed the repo for two different actors.
	for _, act := range []string{"alice", "bob"} {
		actorCtx := actor.WithActor(ctx, act)
		require.Nil(t, store.UpsertRepo(actorCtx, dbgen.Repo{
			Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "https://github.com/my-org/my-repo",
		}))
	}

	raw := makePRPayload(t, "opened", "open", "my-org", "my-repo", 99, "Multi-actor PR")
	event := webhook.ParseEvent("pull_request", raw)
	dispatcher.Dispatch(ctx, event)

	// Both actors should have the PR.
	for _, act := range []string{"alice", "bob"} {
		actorCtx := actor.WithActor(ctx, act)
		pr, err := store.GetPullRequest(actorCtx, "my-org", "my-repo", 99)
		require.Nil(t, err)
		assert.Equal(t, "Multi-actor PR", pr.Title)
	}
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
	actorCtx := actor.WithActor(ctx, "test-user")

	require.Nil(t, store.UpsertRepo(actorCtx, dbgen.Repo{
		Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "u",
	}))
	require.Nil(t, store.UpsertPR(actorCtx, dbgen.PullRequest{
		Owner: "my-org", Repo: "my-repo", Number: 1, Title: "PR", Url: "u",
		State: "OPEN", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		HeadRefOid: sql.NullString{String: "sha1", Valid: true},
	}))
	seed(t, mgr, KindOrgRepos, "my-org")

	dispatcher.Dispatch(ctx, webhook.ParseEvent("status", makeStatusPayload(t, "my-org", "my-repo", "sha1", "success", "ci/build")))

	pr, err := store.GetPullRequest(actorCtx, "my-org", "my-repo", 1)
	require.Nil(t, err)
	assert.Equal(t, "SUCCESS", pr.LastCommitStatus.String)

	// org repos must NOT be invalidated — the rollup was applied directly.
	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateFresh, meta.State)

	// A second, failing context flips the rollup to FAILURE.
	dispatcher.Dispatch(ctx, webhook.ParseEvent("status", makeStatusPayload(t, "my-org", "my-repo", "sha1", "failure", "ci/test")))
	pr, err = store.GetPullRequest(actorCtx, "my-org", "my-repo", 1)
	require.Nil(t, err)
	assert.Equal(t, "FAILURE", pr.LastCommitStatus.String)
}

// TestDispatch_AppliedRecordsDelivery verifies the dispatch returns an "applied"
// result and records it (with the delivery id) in the global webhook log.
func TestDispatch_AppliedRecordsDelivery(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	actorCtx := actor.WithActor(ctx, "alice")
	require.Nil(t, store.UpsertRepo(actorCtx, dbgen.Repo{
		Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "u",
	}))

	event := webhook.ParseEvent("pull_request", makePRPayload(t, "opened", "open", "my-org", "my-repo", 42, "Add feature"))
	event.DeliveryID = "delivery-42"

	result := dispatcher.Dispatch(ctx, event)

	assert.Equal(t, webhook.DispApplied, result.Disposition)
	assert.Equal(t, 1, result.Scopes)
	assert.Equal(t, "my-org/my-repo", result.Repo)
	assert.Equal(t, http.StatusOK, result.StatusCode())

	deliveries, err := store.RecentWebhookDeliveries(ctx, 10)
	require.Nil(t, err)
	require.Len(t, deliveries, 1)
	assert.Equal(t, "delivery-42", deliveries[0].DeliveryID)
	assert.Equal(t, "pull_request", deliveries[0].EventType)
	assert.Equal(t, "opened", deliveries[0].Action)
	assert.Equal(t, webhook.DispApplied, deliveries[0].Disposition)
	assert.Equal(t, int64(1), deliveries[0].Actors)
}

// TestDispatch_SkippedWhenRepoNotCached verifies a parseable event for a repo no
// actor has cached is a no-op recorded as "skipped" (202), not a silent success.
func TestDispatch_SkippedWhenRepoNotCached(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	event := webhook.ParseEvent("pull_request", makePRPayload(t, "opened", "open", "ghost-org", "ghost-repo", 1, "Nobody cached this"))
	result := dispatcher.Dispatch(ctx, event)

	assert.Equal(t, webhook.DispSkipped, result.Disposition)
	assert.Equal(t, 0, result.Scopes)
	assert.Equal(t, http.StatusAccepted, result.StatusCode())

	deliveries, err := store.RecentWebhookDeliveries(ctx, 10)
	require.Nil(t, err)
	require.Len(t, deliveries, 1)
	assert.Equal(t, webhook.DispSkipped, deliveries[0].Disposition)
	assert.Equal(t, "ghost-org/ghost-repo", deliveries[0].Repo)
}

// TestDispatch_IgnoredUntrackedEvent verifies an event the mirror does not track
// (e.g. workflow_job) is recorded as "ignored" rather than dropped invisibly.
func TestDispatch_IgnoredUntrackedEvent(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	event := webhook.Event{Type: "workflow_job", Action: "completed"}
	result := dispatcher.Dispatch(ctx, event)

	assert.Equal(t, webhook.DispIgnored, result.Disposition)
	assert.Equal(t, http.StatusAccepted, result.StatusCode())

	deliveries, err := store.RecentWebhookDeliveries(ctx, 10)
	require.Nil(t, err)
	require.Len(t, deliveries, 1)
	assert.Equal(t, "workflow_job", deliveries[0].EventType)
	assert.Equal(t, webhook.DispIgnored, deliveries[0].Disposition)
}

// TestDispatch_PRUpsert_PreservesStatus verifies a later pull_request webhook
// (which carries no CI status) doesn't wipe a status set by a check webhook.
func TestDispatch_PRUpsert_PreservesStatus(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	actorCtx := actor.WithActor(ctx, "test-user")

	require.Nil(t, store.UpsertRepo(actorCtx, dbgen.Repo{
		Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "u",
	}))
	require.Nil(t, store.UpsertPR(actorCtx, dbgen.PullRequest{
		Owner: "my-org", Repo: "my-repo", Number: 42, Title: "PR", Url: "u",
		State: "OPEN", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		HeadRefOid: sql.NullString{String: "abc123", Valid: true},
	}))

	// CI status arrives first.
	dispatcher.Dispatch(ctx, webhook.ParseEvent("status", makeStatusPayload(t, "my-org", "my-repo", "abc123", "success", "ci")))

	// Then a pull_request webhook (e.g. "labeled") with no CI status.
	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", makePRPayload(t, "labeled", "open", "my-org", "my-repo", 42, "PR")))

	pr, err := store.GetPullRequest(actorCtx, "my-org", "my-repo", 42)
	require.Nil(t, err)
	assert.Equal(t, "SUCCESS", pr.LastCommitStatus.String, "PR upsert must not clobber CI status")
}

func makePushPayload(t *testing.T, owner, repo, ts string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{
		"repository":  map[string]interface{}{"name": repo, "owner": map[string]interface{}{"login": owner}},
		"head_commit": map[string]interface{}{"timestamp": ts},
	})
	require.Nil(t, err)
	return data
}

func TestDispatch_Push_UpdatesPushedAt(t *testing.T) {
	dispatcher, mgr, fStore, store := setupDispatcher(t)
	ctx := context.Background()
	actorCtx := actor.WithActor(ctx, "test-user")

	require.Nil(t, store.UpsertRepo(actorCtx, dbgen.Repo{
		Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "u",
		PushedAt: sql.NullString{String: "2020-01-01T00:00:00Z", Valid: true},
	}))
	seed(t, mgr, KindOrgRepos, "my-org")

	dispatcher.Dispatch(ctx, webhook.ParseEvent("push", makePushPayload(t, "my-org", "my-repo", "2026-05-01T12:00:00Z")))

	repo, err := store.GetRepo(actorCtx, "my-org", "my-repo")
	require.Nil(t, err)
	assert.Equal(t, "2026-05-01T12:00:00Z", repo.PushedAt.String)

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateFresh, meta.State)
}

func TestDispatch_PullRequestReview_AppliesPR(t *testing.T) {
	dispatcher, mgr, fStore, store := setupDispatcher(t)
	ctx := context.Background()
	actorCtx := actor.WithActor(ctx, "test-user")

	require.Nil(t, store.UpsertRepo(actorCtx, dbgen.Repo{
		Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "u",
	}))
	seed(t, mgr, KindOrgRepos, "my-org")

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request_review",
		makePRPayload(t, "submitted", "open", "my-org", "my-repo", 5, "Reviewed PR")))

	pr, err := store.GetPullRequest(actorCtx, "my-org", "my-repo", 5)
	require.Nil(t, err)
	assert.Equal(t, "Reviewed PR", pr.Title)

	meta, err := fStore.Get(ctx, freshness.ResourceID{Kind: KindOrgRepos, Key: "my-org"})
	require.Nil(t, err)
	assert.Equal(t, freshness.StateFresh, meta.State)
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
	actorCtx := actor.WithActor(ctx, "test-user")

	require.Nil(t, store.UpsertRepo(actorCtx, dbgen.Repo{
		Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "u",
	}))
	require.Nil(t, store.SetPRLabels(actorCtx, "my-org", "my-repo", 1, []dbgen.PrLabel{
		{Owner: "my-org", Repo: "my-repo", PrNumber: 1, Name: "bug", Color: "aaaaaa"},
	}))

	dispatcher.Dispatch(ctx, webhook.ParseEvent("label", makeLabelPayload(t, "edited", "my-org", "my-repo", "bug", "bbbbbb")))
	labels, err := store.ListPRLabels(actorCtx, "my-org", "my-repo", 1)
	require.Nil(t, err)
	require.Equal(t, 1, len(labels))
	assert.Equal(t, "bbbbbb", labels[0].Color)

	dispatcher.Dispatch(ctx, webhook.ParseEvent("label", makeLabelPayload(t, "deleted", "my-org", "my-repo", "bug", "bbbbbb")))
	labels, err = store.ListPRLabels(actorCtx, "my-org", "my-repo", 1)
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
	actorCtx := actor.WithActor(ctx, "test-user")

	require.Nil(t, store.UpsertRepo(actorCtx, dbgen.Repo{
		Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "u",
		DefaultBranch: sql.NullString{String: "main", Valid: true},
	}))

	dispatcher.Dispatch(ctx, webhook.ParseEvent("check_suite",
		makeCheckSuitePayload(t, "my-org", "my-repo", "sha9", "main", "main", "success")))
	repo, err := store.GetRepo(actorCtx, "my-org", "my-repo")
	require.Nil(t, err)
	assert.Equal(t, "SUCCESS", repo.DefaultBranchStatus.String)

	dispatcher.Dispatch(ctx, webhook.ParseEvent("check_suite",
		makeCheckSuitePayload(t, "my-org", "my-repo", "sha10", "feature", "main", "failure")))
	repo, err = store.GetRepo(actorCtx, "my-org", "my-repo")
	require.Nil(t, err)
	assert.Equal(t, "SUCCESS", repo.DefaultBranchStatus.String, "non-default branch must not change default_branch_status")
}
