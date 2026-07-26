package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// TestDispatch_CIEventsFlushCommitCICache: status, check_run, and check_suite
// events flush the commit-CI snapshots for exactly the ref spellings the
// payload names -- the head branch(es) plus the sha itself -- while a ref the
// payload does NOT name, and another repo's snapshots, survive (round 2's
// per-ref grain). A push with no usable ref and a repository event keep the
// conservative repo-wide flush.
func TestDispatch_CIEventsFlushCommitCICache(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	now := time.Now()

	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	seedRef := func(repo, ref string) {
		t.Helper()
		for _, kind := range []string{ghdata.CommitCIKindStatus, ghdata.CommitCIKindCheckRuns} {
			require.NoError(t, store.PutCachedCommitCI(ctx, ghdata.CachedCommitCI{
				Owner: "org1", Repo: repo, Ref: ref, Kind: kind,
				Doc: `{"seeded":true}`,
			}, 30, 1, now, time.Hour))
			_, ok, err := store.GetCachedCommitCI(ctx, "org1", repo, ref, kind, 30, 1, now)
			require.NoError(t, err)
			require.True(t, ok, "seeded %s snapshot for %q must serve", kind, ref)
		}
	}
	serves := func(repo, ref, kind string) bool {
		t.Helper()
		_, ok, err := store.GetCachedCommitCI(ctx, "org1", repo, ref, kind, 30, 1, now)
		require.NoError(t, err)
		return ok
	}

	// The CI events name refs: both named spellings (branch + sha) flush,
	// the unnamed ref survives.
	for _, tc := range []struct{ event, body string }{
		{"status", `{"sha":"` + sha + `","state":"success","context":"ci/build",
			"branches":[{"name":"main"}],
			"repository":{"name":"repo1","owner":{"login":"org1"}}}`},
		{"check_run", `{"action":"completed",
			"check_run":{"head_sha":"` + sha + `","status":"completed","conclusion":"success","name":"build",
				"check_suite":{"head_branch":"main"}},
			"repository":{"name":"repo1","owner":{"login":"org1"}}}`},
		{"check_suite", `{"action":"completed",
			"check_suite":{"head_sha":"` + sha + `","head_branch":"main","status":"completed","conclusion":"success"},
			"repository":{"name":"repo1","owner":{"login":"org1"}}}`},
	} {
		seedRef("repo1", "main")
		seedRef("repo1", sha)
		seedRef("repo1", "claude/dev")
		seedRef("other-repo", "main")
		dispatcher.Dispatch(ctx, webhook.ParseEvent(tc.event, []byte(tc.body)))
		for _, kind := range []string{ghdata.CommitCIKindStatus, ghdata.CommitCIKindCheckRuns} {
			assert.False(t, serves("repo1", "main", kind),
				"a %s event must flush the named branch's %s snapshots", tc.event, kind)
			assert.False(t, serves("repo1", sha, kind),
				"a %s event must flush the sha spelling's %s snapshots", tc.event, kind)
			assert.True(t, serves("repo1", "claude/dev", kind),
				"a %s event must leave an unnamed ref's %s snapshots intact", tc.event, kind)
			assert.True(t, serves("other-repo", "main", kind),
				"a %s event must not flush another repo's snapshots", tc.event)
		}
	}

	// A push without a usable ref and a repository event flush repo-wide.
	for _, tc := range []struct{ event, body string }{
		{"push", `{"repository":{"name":"repo1","full_name":"org1/repo1","owner":{"login":"org1"}}}`},
		{"repository", `{"action":"privatized","repository":{"name":"repo1","full_name":"org1/repo1","owner":{"login":"org1"}}}`},
	} {
		seedRef("repo1", "main")
		seedRef("repo1", "claude/dev")
		seedRef("other-repo", "main")
		dispatcher.Dispatch(ctx, webhook.ParseEvent(tc.event, []byte(tc.body)))
		for _, ref := range []string{"main", "claude/dev"} {
			assert.False(t, serves("repo1", ref, ghdata.CommitCIKindStatus),
				"a %s event must flush the repo's %q snapshots", tc.event, ref)
			assert.False(t, serves("repo1", ref, ghdata.CommitCIKindCheckRuns),
				"a %s event must flush the repo's %q snapshots", tc.event, ref)
		}
		assert.True(t, serves("other-repo", "main", ghdata.CommitCIKindStatus),
			"a %s event must not flush another repo's snapshots", tc.event)
	}
}

// TestDispatch_CheckSuite_PendingIgnored: a non-completed check_suite delivery
// must record NO commit_checks row. GitHub auto-creates a suite per sha for
// every app with checks:write, and an app that runs no checks leaves its empty
// suite queued forever -- the PENDING row such a delivery used to mint was a
// permanent ghost no event ever cleared, pinning the low-water-mark rollup at
// PENDING and re-poisoning last_commit_status on every PR upsert (the
// 2026-07-20 report's live-minting rollup cluster). The delivery reports
// ignored while the response-cache flush still runs (invalidation precedes
// the disposition logic -- the queued-workflow_job precedent). Completed
// suites, and PENDING check_run/status events (which DO carry real pending
// state), keep applying exactly as before.
func TestDispatch_CheckSuite_PendingIgnored(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	now := time.Now()

	const sha = "dd112845121b15a3ed9c6be8f98494e567df4eb7"
	suite := func(action, status, conclusion string) json.RawMessage {
		cs := map[string]interface{}{
			"head_sha": sha, "head_branch": "main", "status": status,
			"app": map[string]interface{}{"slug": "cloudflare-workers-and-pages"},
		}
		if conclusion != "" {
			cs["conclusion"] = conclusion
		}
		raw, err := json.Marshal(map[string]interface{}{
			"action":      action,
			"check_suite": cs,
			"repository": map[string]interface{}{
				"name": "my-repo", "default_branch": "main",
				"owner": map[string]interface{}{"login": "my-org"},
			},
		})
		require.NoError(t, err)
		return raw
	}
	seedSnapshot := func() {
		t.Helper()
		require.NoError(t, store.PutCachedCommitCI(ctx, ghdata.CachedCommitCI{
			Owner: "my-org", Repo: "my-repo", Ref: sha, Kind: ghdata.CommitCIKindCheckRuns,
			Doc: `{"seeded":true}`,
		}, 30, 1, now, time.Hour))
	}
	snapshotServes := func() bool {
		t.Helper()
		_, ok, err := store.GetCachedCommitCI(ctx, "my-org", "my-repo", sha, ghdata.CommitCIKindCheckRuns, 30, 1, now)
		require.NoError(t, err)
		return ok
	}

	// A PR heads the sha: the ghost row would re-poison its rollup forever.
	require.NoError(t, store.UpsertPR(ctx, dbgen.PullRequest{
		Owner: "my-org", Repo: "my-repo", Number: 7, Title: "PR", Url: "u",
		State: "OPEN", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		HeadRefOid: sql.NullString{String: sha, Valid: true},
	}, now))

	// Each non-completed shape (an auto-created suite's whole lifecycle) is
	// ignored: no row, no rollup, and the snapshot flush still ran.
	for _, tc := range []struct{ action, status, conclusion string }{
		{"requested", "queued", ""},
		{"rerequested", "in_progress", ""},
		{"completed", "completed", "weird_future_conclusion"}, // normalizes PENDING: a finished suite's row would be just as permanent
	} {
		seedSnapshot()
		res := dispatcher.Dispatch(ctx, webhook.ParseEvent("check_suite", suite(tc.action, tc.status, tc.conclusion)))
		assert.Equal(t, webhook.DispIgnored, res.Disposition, "%s/%s suite must be ignored", tc.action, tc.status)
		assert.Contains(t, res.Detail, "check_suite:cloudflare-workers-and-pages")

		states, err := store.CommitCheckStates(ctx, "my-org", "my-repo", sha)
		require.NoError(t, err)
		assert.Empty(t, states, "a pending suite must mint no commit_checks row")
		pr, err := store.GetPullRequest(ctx, "my-org", "my-repo", 7)
		require.NoError(t, err)
		assert.False(t, pr.LastCommitStatus.Valid, "no ghost PENDING rollup on the PR")
		assert.False(t, snapshotServes(), "the response-cache flush must still run for an ignored pending suite")
	}

	// A PENDING check_run still applies -- real pending state rides run events,
	// which is exactly why dropping pending SUITES loses nothing.
	res := dispatcher.Dispatch(ctx, webhook.ParseEvent("check_run", []byte(`{"action":"created",
		"check_run":{"head_sha":"`+sha+`","status":"queued","name":"build","check_suite":{"head_branch":"main"}},
		"repository":{"name":"my-repo","owner":{"login":"my-org"}}}`)))
	assert.Equal(t, webhook.DispApplied, res.Disposition, "a queued check_run is real pending state and must apply")
	pr, err := store.GetPullRequest(ctx, "my-org", "my-repo", 7)
	require.NoError(t, err)
	assert.Equal(t, "PENDING", pr.LastCommitStatus.String)

	// A COMPLETED suite with a real conclusion applies exactly as before --
	// and its terminal state wins the rollup over the queued run.
	res = dispatcher.Dispatch(ctx, webhook.ParseEvent("check_suite", suite("completed", "completed", "failure")))
	assert.Equal(t, webhook.DispApplied, res.Disposition)
	states, err := store.CommitCheckStates(ctx, "my-org", "my-repo", sha)
	require.NoError(t, err)
	assert.Contains(t, states, "FAILURE", "a completed suite must still record its row")
	pr, err = store.GetPullRequest(ctx, "my-org", "my-repo", 7)
	require.NoError(t, err)
	assert.Equal(t, "FAILURE", pr.LastCommitStatus.String)
}
