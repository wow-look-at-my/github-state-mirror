package ghclient

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func TestGetOrgData_BasicQuery(t *testing.T) {
	pushedAt := "2024-01-15T10:00:00Z"
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/graphql", r.URL.Path)
		assert.Equal(t, "POST", r.Method)

		resp := gqlResponse{}
		resp.Data.Organization.Repositories.PageInfo = gqlPageInfo{HasNextPage: false}
		resp.Data.Organization.Repositories.Nodes = []gqlRepo{
			{
				Name:          "my-repo",
				NameWithOwner: "myorg/my-repo",
				URL:           "https://github.com/myorg/my-repo",
				IsDisabled:    false,
				PushedAt:      &pushedAt,
				Owner: struct {
					Login     string `json:"login"`
					AvatarURL string `json:"avatarUrl"`
					URL       string `json:"url"`
				}{Login: "myorg", AvatarURL: "http://avatar", URL: "http://url"},
				DefaultBranchRef: &struct {
					Name   string `json:"name"`
					Target struct {
						StatusCheckRollup *struct {
							State string `json:"state"`
						} `json:"statusCheckRollup"`
					} `json:"target"`
				}{
					Name: "main",
					Target: struct {
						StatusCheckRollup *struct {
							State string `json:"state"`
						} `json:"statusCheckRollup"`
					}{
						StatusCheckRollup: &struct {
							State string `json:"state"`
						}{State: "SUCCESS"},
					},
				},
				PullRequests: struct {
					PageInfo gqlPageInfo `json:"pageInfo"`
					Nodes    []gqlPR     `json:"nodes"`
				}{
					PageInfo: gqlPageInfo{HasNextPage: false},
					Nodes: []gqlPR{
						{
							Number:      1,
							Title:       "Fix bug",
							URL:         "https://github.com/myorg/my-repo/pull/1",
							IsDraft:     false,
							CreatedAt:   "2024-01-10T10:00:00Z",
							UpdatedAt:   "2024-01-15T10:00:00Z",
							Additions:   10,
							Deletions:   5,
							Mergeable:   "MERGEABLE",
							HeadRefName: "fix-bug",
							BaseRefName: "main",
							HeadRefOid:  "abc123",
							Author: &struct {
								Login     string `json:"login"`
								AvatarURL string `json:"avatarUrl"`
								URL       string `json:"url"`
							}{Login: "dev", AvatarURL: "http://dev-avatar", URL: "http://dev-url"},
							Labels: struct {
								Nodes []struct {
									Name  string `json:"name"`
									Color string `json:"color"`
								} `json:"nodes"`
							}{
								Nodes: []struct {
									Name  string `json:"name"`
									Color string `json:"color"`
								}{
									{Name: "bug", Color: "d73a4a"},
								},
							},
							ReviewRequests: struct {
								TotalCount int `json:"totalCount"`
							}{TotalCount: 2},
							Commits: struct {
								Nodes []struct {
									Commit struct {
										StatusCheckRollup *struct {
											State string `json:"state"`
										} `json:"statusCheckRollup"`
									} `json:"commit"`
								} `json:"nodes"`
							}{
								Nodes: []struct {
									Commit struct {
										StatusCheckRollup *struct {
											State string `json:"state"`
										} `json:"statusCheckRollup"`
									} `json:"commit"`
								}{
									{
										Commit: struct {
											StatusCheckRollup *struct {
												State string `json:"state"`
											} `json:"statusCheckRollup"`
										}{
											StatusCheckRollup: &struct {
												State string `json:"state"`
											}{State: "SUCCESS"},
										},
									},
								},
							},
						},
					},
				},
			},
		}

		json.NewEncoder(w).Encode(resp)
	})

	data, err := c.GetOrgData(context.Background(), "myorg")
	require.NoError(t, err)
	require.Equal(t, 1, len(data.Repos))

	repo := data.Repos[0]
	assert.Equal(t, "my-repo", repo.Name)
	assert.Equal(t, "myorg/my-repo", repo.NameWithOwner)
	assert.Equal(t, "main", repo.DefaultBranch.String)
	assert.Equal(t, "SUCCESS", repo.DefaultBranchStatus.String)

	prs := data.PRsByRepo["myorg/my-repo"]
	require.Equal(t, 1, len(prs))
	assert.Equal(t, int64(1), prs[0].Number)
	assert.Equal(t, "Fix bug", prs[0].Title)
	assert.Equal(t, "dev", prs[0].AuthorLogin.String)
	assert.Equal(t, "fix-bug", prs[0].HeadRefName.String)
	assert.Equal(t, "SUCCESS", prs[0].LastCommitStatus.String)

	labels := data.LabelsByPR["myorg/my-repo"][1]
	require.Equal(t, 1, len(labels))
	assert.Equal(t, "bug", labels[0].Name)
}

func TestGetOrgData_GraphQLError(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data":   nil,
			"errors": []map[string]string{{"message": "org not found"}},
		}
		json.NewEncoder(w).Encode(resp)
	})

	_, err := c.GetOrgData(context.Background(), "nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "org not found")
}

func TestGetOrgData_Pagination(t *testing.T) {
	callCount := 0
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := gqlResponse{}
		if callCount == 1 {
			resp.Data.Organization.Repositories.PageInfo = gqlPageInfo{HasNextPage: true, EndCursor: "cursor1"}
			resp.Data.Organization.Repositories.Nodes = []gqlRepo{
				{Name: "repo1", NameWithOwner: "org/repo1"},
			}
		} else {
			resp.Data.Organization.Repositories.PageInfo = gqlPageInfo{HasNextPage: false}
			resp.Data.Organization.Repositories.Nodes = []gqlRepo{
				{Name: "repo2", NameWithOwner: "org/repo2"},
			}
		}
		json.NewEncoder(w).Encode(resp)
	})

	data, err := c.GetOrgData(context.Background(), "org")
	require.NoError(t, err)
	assert.Equal(t, 2, len(data.Repos))
	assert.Equal(t, 2, callCount)
}

func TestBoolToInt(t *testing.T) {
	assert.Equal(t, int64(1), boolToInt(true))
	assert.Equal(t, int64(0), boolToInt(false))
}
