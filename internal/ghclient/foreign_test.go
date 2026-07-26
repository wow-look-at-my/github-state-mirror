package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The collaborator-repo bleed guard: GitHub's repositoryOwner
// repositories(...) connection defaults ownerAffiliations to
// [OWNER, COLLABORATOR], so a User's listing can include repos the login
// merely collaborates on -- under their real owners -- which convertRepo/
// convertPR then keyed by the QUERY login, poisoning truth with junk
// "<queried>/<name>" rows. The owner-agnostic queries pin
// ownerAffiliations: OWNER, and every conversion loop drops (never re-keys)
// any node whose real owner differs from the queried login.

// captureSlog routes the default slog output into a buffer for the duration
// of the test, so drop logging is assertable.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestOwnerQueriesPinOwnerAffiliation locks the fetch-side half of the fix:
// both checker/sync-private owner queries request ONLY owned repos, while the
// identity-locked org query stays byte-untouched (its protection is purely
// the client-side drop guard).
func TestOwnerQueriesPinOwnerAffiliation(t *testing.T) {
	assert.Contains(t, ownerDataQuery, "ownerAffiliations: OWNER")
	assert.Contains(t, ownerRepoVisibilityQuery, "ownerAffiliations: OWNER")
	assert.NotContains(t, orgDataQuery, "ownerAffiliations",
		"the identity-locked org query must not change (client-side filtering only)")
}

// TestGetOwnerData_DropsForeignOwnerNode: a foreign-owner node in the owner
// listing is dropped with its PRs (no key under the real owner, no re-key
// under the queried login, no follow-up PR pagination), and the drop is
// logged with the node's real identity.
func TestGetOwnerData_DropsForeignOwnerNode(t *testing.T) {
	logs := captureSlog(t)
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Contains(t, req.Query, "repositoryOwner")

		// The foreign node advertises MORE PR pages: were it not dropped
		// before pagination, the per-repo follow-up query above would fire.
		foreign := ownerRepoNode("wow-look-at-my", "tool", nil)
		foreign["pullRequests"] = map[string]any{
			"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "PC"},
			"nodes":    []map[string]any{ownerPRNode(21, nil, "")},
		}
		_ = json.NewEncoder(w).Encode(ownerPage(false, "",
			ownerRepoNode("someuser", "dots", []map[string]any{ownerPRNode(1, nil, "")}),
			foreign))
	})

	data, err := c.GetOwnerData(context.Background(), "someuser")
	require.NoError(t, err)

	require.Len(t, data.Repos, 1, "the foreign collaborator node must be dropped")
	assert.Equal(t, "someuser", data.Repos[0].Owner)
	assert.Equal(t, "dots", data.Repos[0].Name)
	assert.Len(t, data.PRsByRepo["someuser/dots"], 1)
	assert.NotContains(t, data.PRsByRepo, "wow-look-at-my/tool", "the foreign node's PRs are dropped with it")
	assert.NotContains(t, data.PRsByRepo, "someuser/tool", "and never re-keyed under the queried login")
	assert.NotContains(t, data.LabelsByPR, "wow-look-at-my/tool")

	assert.Contains(t, logs.String(), "wow-look-at-my/tool", "the drop must log the node's real identity")
	assert.Contains(t, logs.String(), "someuser", "...and the queried owner")
}

// TestGetOwnerData_ForeignGuardCaseInsensitive: GitHub logins are
// case-insensitive, so a node whose nameWithOwner casing differs from the
// queried login is NOT foreign.
func TestGetOwnerData_ForeignGuardCaseInsensitive(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ownerPage(false, "", ownerRepoNode("SomeUser", "dots", nil)))
	})
	data, err := c.GetOwnerData(context.Background(), "someuser")
	require.NoError(t, err)
	require.Len(t, data.Repos, 1, "an owned repo with different login casing must be kept")
	assert.Equal(t, "dots", data.Repos[0].Name)
}

// TestGetOrgData_DropsForeignOwnerNode: the identity-locked org query cannot
// gain an ownerAffiliations argument (see TestOrgQueryUntouched), so its
// protection is purely the client-side guard: a synthetic foreign node in the
// org listing is dropped with its PRs, and logged.
func TestGetOrgData_DropsForeignOwnerNode(t *testing.T) {
	logs := captureSlog(t)
	c := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		resp := gqlResponse{}
		resp.Data.Organization.Repositories.PageInfo = gqlPageInfo{HasNextPage: false}
		owned := gqlRepo{Name: "repo1", NameWithOwner: "myorg/repo1"}
		foreign := gqlRepo{Name: "collab", NameWithOwner: "otherorg/collab"}
		foreign.PullRequests.Nodes = []gqlPR{{Number: 7, Title: "foreign PR", URL: "u"}}
		resp.Data.Organization.Repositories.Nodes = []gqlRepo{owned, foreign}
		_ = json.NewEncoder(w).Encode(resp)
	})

	data, err := c.GetOrgData(context.Background(), "myorg")
	require.NoError(t, err)
	require.Len(t, data.Repos, 1)
	assert.Equal(t, "repo1", data.Repos[0].Name)
	assert.NotContains(t, data.PRsByRepo, "otherorg/collab")
	assert.NotContains(t, data.PRsByRepo, "myorg/collab", "never re-keyed under the queried org")
	assert.Contains(t, logs.String(), "otherorg/collab")
}

// TestOwnerRepoVisibilities_DropsForeignOwnerNode: the visibility map is
// keyed by BARE name, so a foreign collaborator node with the same bare name
// as an owned repo would otherwise clobber the owned entry (last write wins)
// -- including flipping a private repo's recorded visibility. The guard drops
// foreign nodes before they reach the map.
func TestOwnerRepoVisibilities_DropsForeignOwnerNode(t *testing.T) {
	logs := captureSlog(t)
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), "ownerAffiliations: OWNER",
			"the visibility twin must pin the OWNER affiliation")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"repositoryOwner": map[string]any{
					"repositories": map[string]any{
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						"nodes": []map[string]any{
							{"name": "dots", "nameWithOwner": "someuser/dots", "visibility": "PRIVATE", "isArchived": false},
							{"name": "tool", "nameWithOwner": "wow-look-at-my/tool", "visibility": "PUBLIC", "isArchived": false},
							// Foreign node with the SAME bare name as the owned
							// one, listed after it: without the guard, last write
							// wins and PUBLIC clobbers the owned PRIVATE entry.
							{"name": "dots", "nameWithOwner": "otherorg/dots", "visibility": "PUBLIC", "isArchived": false},
						},
					},
				},
			},
		})
	})

	vis, err := c.OwnerRepoVisibilities(context.Background(), "someuser")
	require.NoError(t, err)
	require.Len(t, vis, 1)
	assert.Equal(t, OwnerRepoVisibility{Visibility: "private"}, vis["dots"],
		"a same-named foreign node must not clobber the owned entry")
	assert.NotContains(t, vis, "tool")
	assert.Contains(t, logs.String(), "wow-look-at-my/tool")
	assert.Contains(t, logs.String(), "otherorg/dots")
}
