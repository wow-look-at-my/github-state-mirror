package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// Dispatcher tests for the pull_request and status events -- the payloads
// that apply straight to PR truth -- plus the delivery-log records every
// dispatch leaves behind.

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

// TestDispatch_PullRequest_NullMergeableCoalesces: a PR payload carrying
// mergeable:null (GitHub still computing) must not clobber a known cached
// value -- the upsert COALESCEs it. The un-resolve signal is the PUSH event
// (see TestDispatch_Push_UnresolvesMergeableByBranch), not the PR event's
// transient null.
func TestDispatch_PullRequest_NullMergeableCoalesces(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()

	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", makePRPayload(t, "opened", "open", "my-org", "my-repo", 42, "PR")))
	pr, err := store.GetPullRequest(ctx, "my-org", "my-repo", 42)
	require.NoError(t, err)
	require.True(t, pr.Mergeable.Valid, "the opened payload carried a resolved mergeable")

	// Same PR, synchronize, mergeable null (recomputing).
	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(makePRPayload(t, "synchronize", "open", "my-org", "my-repo", 42, "PR"), &payload))
	payload["pull_request"].(map[string]interface{})["mergeable"] = nil
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	dispatcher.Dispatch(ctx, webhook.ParseEvent("pull_request", raw))

	pr, err = store.GetPullRequest(ctx, "my-org", "my-repo", 42)
	require.NoError(t, err)
	assert.True(t, pr.Mergeable.Valid, "a null-mergeable payload must not clobber the known value")
	assert.Equal(t, "MERGEABLE", pr.Mergeable.String)
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

// TestDispatch_Push_DefaultBranchNullsStatus: a push to the DEFAULT branch
// un-resolves the repo's default_branch_status -- the stored rollup describes
// the previous tip, and a tip with no CI would otherwise keep it forever (the
// COALESCE upsert can never clear it). A non-default-branch push leaves it.
func TestDispatch_Push_DefaultBranchNullsStatus(t *testing.T) {
	dispatcher, mgr, _, store := setupDispatcher(t)
	ctx := context.Background()

	seedRepo := func() {
		require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
			Owner: "my-org", Name: "my-repo", NameWithOwner: "my-org/my-repo", Url: "u",
			DefaultBranch:       sql.NullString{String: "main", Valid: true},
			DefaultBranchStatus: sql.NullString{String: "SUCCESS", Valid: true},
		}))
	}
	seedRepo()
	seed(t, mgr, KindOrgRepos, "my-org")

	// The push payload's own repository object names the default branch.
	mkPayload := func(ref string) json.RawMessage {
		data, err := json.Marshal(map[string]any{
			"ref": ref,
			"repository": map[string]any{
				"name": "my-repo", "default_branch": "main",
				"owner": map[string]any{"login": "my-org"},
			},
			"head_commit": map[string]any{"timestamp": "2026-05-01T12:00:00Z"},
		})
		require.NoError(t, err)
		return data
	}

	// A feature-branch push leaves the default-branch rollup alone.
	dispatcher.Dispatch(ctx, webhook.ParseEvent("push", mkPayload("refs/heads/feature")))
	repo, err := store.GetRepo(ctx, "my-org", "my-repo")
	require.NoError(t, err)
	assert.Equal(t, "SUCCESS", repo.DefaultBranchStatus.String, "non-default push must not clear the rollup")

	// A default-branch push un-resolves it.
	dispatcher.Dispatch(ctx, webhook.ParseEvent("push", mkPayload("refs/heads/main")))
	repo, err = store.GetRepo(ctx, "my-org", "my-repo")
	require.NoError(t, err)
	assert.False(t, repo.DefaultBranchStatus.Valid, "default-branch push must un-resolve the stale rollup")

	// Payload without default_branch: falls back to the cached row's value.
	seedRepo()
	dispatcher.Dispatch(ctx, webhook.ParseEvent("push", makePushPayload(t, "my-org", "my-repo", "refs/heads/main", "2026-05-01T12:00:00Z")))
	repo, err = store.GetRepo(ctx, "my-org", "my-repo")
	require.NoError(t, err)
	assert.False(t, repo.DefaultBranchStatus.Valid, "cached default_branch backs the payload-less case")
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

// mustJSON marshals a delivery body. Every payload in this package's tests is
// built this way rather than spliced into a JSON literal: a value between the
// literal's own quotes is escaped by nothing, and internal/guards' json-splice
// check fails the build over it.
func mustJSON(t *testing.T, payload map[string]any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	return data
}
