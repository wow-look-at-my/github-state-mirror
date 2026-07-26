package sync

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConsistencyChecker_ForeignCollaboratorNodesDropped: a User owner's
// repositoryOwner listing can include repos the login merely collaborates on
// (GitHub's [OWNER, COLLABORATOR] ownerAffiliations default) -- under their
// real owners. The client-side guard drops those nodes, so the checker must
// neither report nor (in apply mode, via SyncOrgTruth) absorb anything keyed
// "<queried>/<foreign name>" -- the junk rows the collaborator-repo bleed
// used to create. The foreign name must be absent from the visibility map
// path too.
func TestConsistencyChecker_ForeignCollaboratorNodesDropped(t *testing.T) {
	srv := consistencyFakeGitHub(t, map[string]fakeOwner{
		"someuser": {
			repos: []map[string]any{
				liveRepo("someuser", "dots", "SUCCESS", nil),
				// The foreign collaborator node, self-identified by its
				// nameWithOwner/owner.login, carrying an open PR.
				liveRepo("wow-look-at-my", "tool", "SUCCESS",
					[]map[string]any{livePR(21, "sha21", "SUCCESS", "")}),
			},
			vis: []map[string]any{
				visNode("dots", "PUBLIC", false),
				// The same foreign repo in the visibility twin.
				{"name": "tool", "nameWithOwner": "wow-look-at-my/tool", "visibility": "PUBLIC", "isArchived": false},
			},
		},
	})
	checker, store, _ := newCheckerTest(t, srv.URL)
	ctx := context.Background()

	rep, err := checker.CheckAndApply(ctx, "someuser")
	require.NoError(t, err)

	// The report may not carry a single foreign-keyed entry -- neither under
	// the queried login nor under the real owner (dropped, not re-keyed).
	for _, d := range rep.Discrepancies {
		assert.NotContains(t, d.Repo, "/tool",
			"foreign collaborator repo must not appear in the report: %+v", d)
	}
	assert.NotNil(t, findDiscrepancy(rep, "someuser/dots", "", "only_on_github"),
		"the owned repo still diffs normally")

	// Apply absorbed the owned repo but nothing under the foreign key.
	dots, err := store.GetRepo(ctx, "someuser", "dots")
	require.NoError(t, err, "the owned repo must be absorbed")
	assert.Equal(t, "public", dots.Visibility, "owned visibility applies from the (guarded) map")
	_, err = store.GetRepo(ctx, "someuser", "tool")
	assert.ErrorIs(t, err, sql.ErrNoRows,
		"SyncOrgTruth must absorb nothing keyed by the queried login for a foreign node")
	_, err = store.GetRepo(ctx, "wow-look-at-my", "tool")
	assert.ErrorIs(t, err, sql.ErrNoRows,
		"...and nothing re-keyed under the real owner either (dropped, not re-keyed)")
	_, err = store.GetPullRequest(ctx, "someuser", "tool", 21)
	assert.ErrorIs(t, err, sql.ErrNoRows, "the foreign node's PRs are dropped with it")

	// The junk rows were self-identifying (name_with_owner != owner/name);
	// everything absorbed now satisfies the invariant.
	repos, err := store.AllRepos(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, repos)
	for _, r := range repos {
		assert.Equal(t, r.Owner+"/"+r.Name, r.NameWithOwner,
			"absorbed truth rows must be keyed by their real owner")
	}
}
