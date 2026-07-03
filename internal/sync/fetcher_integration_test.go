package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

func setupFetcherTest(t *testing.T, handler http.Handler) (*ghclient.Client, *ghdata.Store) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.Nil(t, err)
	t.Cleanup(func() { db.Close() })

	client := ghclient.NewWithBaseURL(srv.URL)
	store := ghdata.NewStore(db)
	return client, store
}

func TestOrgReposFetcher_Fetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"organization": map[string]interface{}{
					"repositories": map[string]interface{}{
						"pageInfo": map[string]interface{}{
							"hasNextPage": false,
							"endCursor":   "",
						},
						"nodes": []map[string]interface{}{
							{
								"name":          "repo1",
								"nameWithOwner": "org1/repo1",
								"url":           "https://github.com/org1/repo1",
								"isDisabled":    false,
								"pushedAt":      "2024-01-01T00:00:00Z",
								"owner": map[string]string{
									"login":     "org1",
									"avatarUrl": "https://a",
									"url":       "https://u",
								},
								"pullRequests": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{
										{
											"number":      1,
											"title":       "Test PR",
											"url":         "https://github.com/org1/repo1/pull/1",
											"isDraft":     false,
											"createdAt":   "2024-01-01",
											"updatedAt":   "2024-01-02",
											"additions":   10,
											"deletions":   5,
											"mergeable":   "MERGEABLE",
											"headRefName": "feature",
											"baseRefName": "main",
											"headRefOid":  "abc123",
											"author": map[string]string{
												"login":     "dev",
												"avatarUrl": "https://a",
												"url":       "https://u",
											},
											"labels": map[string]interface{}{
												"nodes": []map[string]string{
													{"name": "bug", "color": "d73a4a"},
												},
											},
											"reviewRequests": map[string]int{"totalCount": 1},
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

	// The fetch runs as a principal: the snapshot lands in GLOBAL truth and
	// the principal earns a list_sync grant for every repo GitHub returned.
	ctx := actor.WithActor(context.Background(), "user:900")
	result, err := f.Fetch(ctx, "org1", "")
	require.NoError(t, err)
	assert.Equal(t, 2, result.RecordsChanged) // 1 repo + 1 PR

	repos, err := store.ListReposByOwner(context.Background(), "org1")
	require.NoError(t, err)
	require.Equal(t, 1, len(repos))
	assert.Equal(t, "repo1", repos[0].Name)

	prs, err := store.ListOpenPRsByRepo(context.Background(), "org1", "repo1")
	require.NoError(t, err)
	require.Equal(t, 1, len(prs))
	assert.Equal(t, "Test PR", prs[0].Title)

	ok, err := store.HasGrant(context.Background(), "user:900", "org1", "repo1", time.Now())
	require.NoError(t, err)
	assert.True(t, ok, "list sync must record a grant for each repo GitHub returned")
}

func TestOrgReposFetcher_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	})

	client, store := setupFetcherTest(t, mux)
	f := &OrgReposFetcher{gh: client, store: store}

	_, err := f.Fetch(actor.WithActor(context.Background(), "user:900"), "org1", "")
	assert.Error(t, err)
}
