package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
