package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupFetcherTest(t *testing.T, handler http.Handler) (*ghclient.Client, *ghdata.Store) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)
	t.Cleanup(func() { db.Close() })

	client := ghclient.NewWithBaseURL("test-token", srv.URL)
	store := ghdata.NewStore(db)
	return client, store
}

func TestUserFetcher_Fetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"login":	"octocat",
			"avatar_url":	"https://avatar",
			"html_url":	"https://github.com/octocat",
		})
	})

	client, store := setupFetcherTest(t, mux)
	f := &UserFetcher{gh: client, store: store}

	result, err := f.Fetch(context.Background(), "self", "")
	require.NoError(t, err)
	assert.Equal(t, 1, result.RecordsChanged)

	user, err := store.GetFirstUser(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "octocat", user.Login)
}

func TestUserOrgsFetcher_Fetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{
			{"login": "org1", "avatar_url": "https://a1", "url": "https://u1"},
			{"login": "org2", "avatar_url": "https://a2", "url": "https://u2"},
		})
	})

	client, store := setupFetcherTest(t, mux)
	f := &UserOrgsFetcher{gh: client, store: store}

	result, err := f.Fetch(context.Background(), "octocat", "")
	require.NoError(t, err)
	assert.Equal(t, 2, result.RecordsChanged)

	orgs, err := store.ListUserOrgs(context.Background(), "octocat")
	require.NoError(t, err)
	assert.Equal(t, 2, len(orgs))
}

func TestPRFilesFetcher_Fetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org1/repo1/pulls/42/files", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"filename": "main.go", "additions": 10, "deletions": 5},
			{"filename": "test.go", "additions": 20, "deletions": 0},
		})
	})

	client, store := setupFetcherTest(t, mux)
	f := &PRFilesFetcher{gh: client, store: store}

	result, err := f.Fetch(context.Background(), "org1/repo1/42", "")
	require.NoError(t, err)
	assert.Equal(t, 2, result.RecordsChanged)

	files, err := store.ListPRFiles(context.Background(), "org1", "repo1", 42)
	require.NoError(t, err)
	assert.Equal(t, 2, len(files))
	assert.Equal(t, "main.go", files[0].Path)
}

func TestCompareFetcher_Fetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org1/repo1/compare/main...feature", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ahead_by":	5,
			"behind_by":	2,
		})
	})

	client, store := setupFetcherTest(t, mux)
	f := &CompareFetcher{gh: client, store: store}

	result, err := f.Fetch(context.Background(), "org1/repo1/main...feature", "")
	require.NoError(t, err)
	assert.Equal(t, 1, result.RecordsChanged)

	comp, err := store.GetComparison(context.Background(), "org1", "repo1", "main", "feature")
	require.NoError(t, err)
	assert.Equal(t, int64(5), comp.AheadBy)
	assert.Equal(t, int64(2), comp.BehindBy)
}

func TestOrgReposFetcher_Fetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"organization": map[string]interface{}{
					"repositories": map[string]interface{}{
						"pageInfo": map[string]interface{}{
							"hasNextPage":	false,
							"endCursor":	"",
						},
						"nodes": []map[string]interface{}{
							{
								"name":			"repo1",
								"nameWithOwner":	"org1/repo1",
								"url":			"https://github.com/org1/repo1",
								"isDisabled":		false,
								"pushedAt":		"2024-01-01T00:00:00Z",
								"owner": map[string]string{
									"login":	"org1",
									"avatarUrl":	"https://a",
									"url":		"https://u",
								},
								"pullRequests": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{
										{
											"number":	1,
											"title":	"Test PR",
											"url":		"https://github.com/org1/repo1/pull/1",
											"isDraft":	false,
											"createdAt":	"2024-01-01",
											"updatedAt":	"2024-01-02",
											"additions":	10,
											"deletions":	5,
											"mergeable":	"MERGEABLE",
											"headRefName":	"feature",
											"baseRefName":	"main",
											"headRefOid":	"abc123",
											"author": map[string]string{
												"login":	"dev",
												"avatarUrl":	"https://a",
												"url":		"https://u",
											},
											"labels": map[string]interface{}{
												"nodes": []map[string]string{
													{"name": "bug", "color": "d73a4a"},
												},
											},
											"reviewRequests":	map[string]int{"totalCount": 1},
											"commits": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"commit": map[string]interface{}{
															"statusCheckRollup": map[string]string{
																"state": "SUCCESS",
															},
														},
													},
												},
											},
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

	client, store := setupFetcherTest(t, mux)
	f := &OrgReposFetcher{gh: client, store: store}

	result, err := f.Fetch(context.Background(), "org1", "")
	require.NoError(t, err)
	assert.Equal(t, 2, result.RecordsChanged)	// 1 repo + 1 PR

	repos, err := store.ListReposByOwner(context.Background(), "org1")
	require.NoError(t, err)
	require.Equal(t, 1, len(repos))
	assert.Equal(t, "repo1", repos[0].Name)

	prs, err := store.ListOpenPRsByRepo(context.Background(), "org1", "repo1")
	require.NoError(t, err)
	require.Equal(t, 1, len(prs))
	assert.Equal(t, "Test PR", prs[0].Title)
}

func TestUserFetcher_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	})

	client, store := setupFetcherTest(t, mux)
	f := &UserFetcher{gh: client, store: store}

	_, err := f.Fetch(context.Background(), "self", "")
	assert.Error(t, err)
}
