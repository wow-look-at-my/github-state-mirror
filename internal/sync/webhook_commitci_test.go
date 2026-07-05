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
// events (CI state moved somewhere in the repo), push (branch-form refs'
// tips moved), and repository events all flush BOTH of a repo's commit-CI
// snapshot kinds -- while another repo's snapshots survive (the flush is
// per-repo, not global).
func TestDispatch_CIEventsFlushCommitCICache(t *testing.T) {
	dispatcher, _, _, store := setupDispatcher(t)
	ctx := context.Background()
	now := time.Now()

	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	seedBoth := func(repo string) {
		t.Helper()
		for _, kind := range []string{ghdata.CommitCIKindStatus, ghdata.CommitCIKindCheckRuns} {
			require.NoError(t, store.PutCachedCommitCI(ctx, ghdata.CachedCommitCI{
				Owner: "org1", Repo: repo, Ref: "claude/dev", Kind: kind,
				Doc: `{"seeded":true}`,
			}, now, time.Hour))
			_, ok, err := store.GetCachedCommitCI(ctx, "org1", repo, "claude/dev", kind, now)
			require.NoError(t, err)
			require.True(t, ok, "seeded %s snapshot must serve", kind)
		}
	}
	serves := func(repo, kind string) bool {
		t.Helper()
		_, ok, err := store.GetCachedCommitCI(ctx, "org1", repo, "claude/dev", kind, now)
		require.NoError(t, err)
		return ok
	}

	for _, tc := range []struct{ event, body string }{
		{"status", `{"sha":"` + sha + `","state":"success","context":"ci/build",
			"repository":{"name":"repo1","owner":{"login":"org1"}}}`},
		{"check_run", `{"action":"completed",
			"check_run":{"head_sha":"` + sha + `","status":"completed","conclusion":"success","name":"build"},
			"repository":{"name":"repo1","owner":{"login":"org1"}}}`},
		{"check_suite", `{"action":"completed",
			"check_suite":{"head_sha":"` + sha + `","head_branch":"main","status":"completed","conclusion":"success"},
			"repository":{"name":"repo1","owner":{"login":"org1"}}}`},
		{"push", `{"repository":{"name":"repo1","full_name":"org1/repo1","owner":{"login":"org1"}}}`},
		{"repository", `{"action":"privatized","repository":{"name":"repo1","full_name":"org1/repo1","owner":{"login":"org1"}}}`},
	} {
		seedBoth("repo1")
		seedBoth("other-repo")
		dispatcher.Dispatch(ctx, webhook.ParseEvent(tc.event, []byte(tc.body)))
		assert.False(t, serves("repo1", ghdata.CommitCIKindStatus),
			"a %s event must flush the repo's status snapshots", tc.event)
		assert.False(t, serves("repo1", ghdata.CommitCIKindCheckRuns),
			"a %s event must flush the repo's check-runs snapshots", tc.event)
		assert.True(t, serves("other-repo", ghdata.CommitCIKindStatus),
			"a %s event must not flush another repo's snapshots", tc.event)
		assert.True(t, serves("other-repo", ghdata.CommitCIKindCheckRuns),
			"a %s event must not flush another repo's snapshots", tc.event)
	}
}
