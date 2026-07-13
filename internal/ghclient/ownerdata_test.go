package ghclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ownerRepoNode builds one repositoryOwner repo node with n open PRs.
func ownerRepoNode(owner, name string, prs []map[string]any) map[string]any {
	if prs == nil {
		prs = []map[string]any{}
	}
	return map[string]any{
		"name":          name,
		"nameWithOwner": owner + "/" + name,
		"url":           "https://github.com/" + owner + "/" + name,
		"isDisabled":    false,
		"isArchived":    false,
		"pushedAt":      "2026-01-01T00:00:00Z",
		"owner":         map[string]string{"login": owner, "avatarUrl": "a", "url": "u"},
		"defaultBranchRef": map[string]any{
			"name":   "main",
			"target": map[string]any{"statusCheckRollup": map[string]string{"state": "SUCCESS"}},
		},
		"pullRequests": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			"nodes":    prs,
		},
	}
}

func ownerPRNode(number int, labels []map[string]string, autoMerge string) map[string]any {
	pr := map[string]any{
		"number": number, "title": fmt.Sprintf("PR %d", number), "url": "https://x/pull",
		"isDraft": false, "createdAt": "2026-01-01", "updatedAt": "2026-01-02",
		"additions": 1, "deletions": 1, "mergeable": "MERGEABLE",
		"headRefName": "feature", "baseRefName": "main", "headRefOid": "abc123",
		"author":         map[string]string{"login": "dev", "avatarUrl": "a", "url": "u"},
		"labels":         map[string]any{"nodes": labels},
		"reviewRequests": map[string]int{"totalCount": 0},
		"commits": map[string]any{"nodes": []map[string]any{
			{"commit": map[string]any{"statusCheckRollup": map[string]string{"state": "SUCCESS"}}},
		}},
	}
	if autoMerge != "" {
		pr["autoMergeRequest"] = map[string]string{"mergeMethod": autoMerge}
	}
	return pr
}

func ownerPage(hasNext bool, cursor string, nodes ...map[string]any) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"repositoryOwner": map[string]any{
				"repositories": map[string]any{
					"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor},
					"nodes":    nodes,
				},
			},
		},
	}
}

// TestGetOwnerData_UserAccountAndFields: the owner-agnostic query resolves a
// User login (the org query cannot) and carries the extra fields -- a >10
// label set, the armed auto-merge method (lowercased from the GraphQL enum),
// and isArchived.
func TestGetOwnerData_UserAccountAndFields(t *testing.T) {
	labels := make([]map[string]string, 0, 12)
	for i := 0; i < 12; i++ {
		labels = append(labels, map[string]string{"name": fmt.Sprintf("label-%02d", i), "color": "ffffff"})
	}
	var sawQuery string
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		sawQuery = req.Query
		assert.Equal(t, "someuser", req.Variables["owner"])
		_ = json.NewEncoder(w).Encode(ownerPage(false, "",
			ownerRepoNode("someuser", "dotfiles", []map[string]any{ownerPRNode(1, labels, "SQUASH")})))
	})

	data, err := c.GetOwnerData(context.Background(), "someuser")
	require.NoError(t, err)

	assert.Contains(t, sawQuery, "repositoryOwner(login: $owner)")
	assert.Contains(t, sawQuery, "labels(first: 100)")
	assert.Contains(t, sawQuery, "autoMergeRequest { mergeMethod }")

	require.Len(t, data.Repos, 1)
	assert.Equal(t, "someuser", data.Repos[0].Owner)
	assert.Equal(t, int64(0), data.Repos[0].IsArchived)

	prs := data.PRsByRepo["someuser/dotfiles"]
	require.Len(t, prs, 1)
	assert.Equal(t, "squash", prs[0].AutoMergeMethod.String, "GraphQL enum lowercased to the REST merge_method value")
	assert.Len(t, data.LabelsByPR["someuser/dotfiles"][1], 12, "labels page at 100, so all 12 survive")
}

// TestGetOwnerData_Pagination: repos page until hasNextPage is false, with the
// cursor threaded through.
func TestGetOwnerData_Pagination(t *testing.T) {
	page := 0
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		page++
		switch page {
		case 1:
			assert.Nil(t, req.Variables["repoCursor"])
			_ = json.NewEncoder(w).Encode(ownerPage(true, "CURSOR1", ownerRepoNode("org1", "repo-a", nil)))
		case 2:
			assert.Equal(t, "CURSOR1", req.Variables["repoCursor"])
			_ = json.NewEncoder(w).Encode(ownerPage(false, "", ownerRepoNode("org1", "repo-b", nil)))
		default:
			t.Fatalf("unexpected page %d", page)
		}
	})

	data, err := c.GetOwnerData(context.Background(), "org1")
	require.NoError(t, err)
	require.Len(t, data.Repos, 2)
	assert.Equal(t, "repo-a", data.Repos[0].Name)
	assert.Equal(t, "repo-b", data.Repos[1].Name)
}

// TestGetOwnerData_PRPagination: a repo whose first PR page reports
// hasNextPage is completed via the owner-agnostic per-repo PR query, and the
// follow-up pages carry the owner-only fields too.
func TestGetOwnerData_PRPagination(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		if strings.Contains(req.Query, "repositoryOwner") {
			node := ownerRepoNode("someuser", "busy", []map[string]any{ownerPRNode(1, nil, "")})
			node["pullRequests"] = map[string]any{
				"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "PR_CURSOR"},
				"nodes":    []map[string]any{ownerPRNode(1, nil, "")},
			}
			_ = json.NewEncoder(w).Encode(ownerPage(false, "", node))
			return
		}
		// The follow-up per-repo PR page.
		assert.Contains(t, req.Query, "autoMergeRequest { mergeMethod }",
			"follow-up PR pages must use the owner selection, not the locked prFields")
		assert.Equal(t, "PR_CURSOR", req.Variables["prCursor"])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"pullRequests": map[string]any{
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						"nodes":    []map[string]any{ownerPRNode(2, nil, "MERGE")},
					},
				},
			},
		})
	})

	data, err := c.GetOwnerData(context.Background(), "someuser")
	require.NoError(t, err)
	prs := data.PRsByRepo["someuser/busy"]
	require.Len(t, prs, 2)
	assert.Equal(t, int64(1), prs[0].Number)
	assert.Equal(t, int64(2), prs[1].Number)
	assert.Equal(t, "merge", prs[1].AutoMergeMethod.String)
}

// TestGetOwnerData_PageHook: the optional per-page callback reports the
// cumulative repos fetched plus the connection's totalCount after EVERY page
// (the owner query selects totalCount, so "N of M" is known from page one).
func TestGetOwnerData_PageHook(t *testing.T) {
	assert.Contains(t, ownerDataQuery, "totalCount", "the owner query must select the connection total for progress reporting")

	pageBody := func(total int, hasNext bool, cursor string, nodes ...map[string]any) map[string]any {
		return map[string]any{
			"data": map[string]any{
				"repositoryOwner": map[string]any{
					"repositories": map[string]any{
						"totalCount": total,
						"pageInfo":   map[string]any{"hasNextPage": hasNext, "endCursor": cursor},
						"nodes":      nodes,
					},
				},
			},
		}
	}
	page := 0
	c := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		page++
		switch page {
		case 1:
			_ = json.NewEncoder(w).Encode(pageBody(3, true, "C1",
				ownerRepoNode("org1", "repo-a", nil), ownerRepoNode("org1", "repo-b", nil)))
		case 2:
			_ = json.NewEncoder(w).Encode(pageBody(3, false, "",
				ownerRepoNode("org1", "repo-c", nil)))
		default:
			t.Fatalf("unexpected page %d", page)
		}
	})

	var calls [][2]int
	data, err := c.GetOwnerDataWithProgress(context.Background(), "org1", func(fetched, total int) {
		calls = append(calls, [2]int{fetched, total})
	})
	require.NoError(t, err)
	require.Len(t, data.Repos, 3)
	assert.Equal(t, [][2]int{{2, 3}, {3, 3}}, calls, "one call per page, cumulative count + connection total")
}

// TestGetOwnerData_NullOwnerFailsLoudly: repositoryOwner is nullable, so an
// unknown login answers data.repositoryOwner=null with NO GraphQL error; that
// must be an error, never an empty (every-repo-is-drift) result.
func TestGetOwnerData_NullOwnerFailsLoudly(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repositoryOwner": nil}})
	})
	_, err := c.GetOwnerData(context.Background(), "ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolved to null")
}

// TestGetOwnerData_GraphQLErrorsNotRetried: GraphQL-level errors[] arrive as
// HTTP 200 semantic answers, not transport blips -- they must fail fast on the
// first attempt (only 502/503/504/429 and network errors are retried).
func TestGetOwnerData_GraphQLErrorsNotRetried(t *testing.T) {
	calls := 0
	c := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]string{{"message": "Something went wrong"}},
		})
	})
	c.SetRetryBackoff([]time.Duration{0})

	_, err := c.GetOwnerData(context.Background(), "org1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "graphql errors")
	assert.Equal(t, 1, calls)
}

// TestOwnerRepoVisibilities: the checker-private visibility twin resolves any
// owner, includes ARCHIVED repos (no isArchived filter), lowercases the
// visibility enum, and paginates.
func TestOwnerRepoVisibilities(t *testing.T) {
	page := 0
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		require.NotContains(t, string(body), "isArchived: false",
			"the visibility twin must NOT filter archived repos")
		page++
		nodes := []map[string]any{
			{"name": "pub", "visibility": "PUBLIC", "isArchived": false},
			{"name": "priv", "visibility": "PRIVATE", "isArchived": false},
		}
		hasNext := true
		cursor := "C1"
		if page == 2 {
			nodes = []map[string]any{
				{"name": "old", "visibility": "PRIVATE", "isArchived": true},
				{"name": "inner", "visibility": "INTERNAL", "isArchived": false},
			}
			hasNext, cursor = false, ""
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"repositoryOwner": map[string]any{
					"repositories": map[string]any{
						"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor},
						"nodes":    nodes,
					},
				},
			},
		})
	})

	vis, err := c.OwnerRepoVisibilities(context.Background(), "someuser")
	require.NoError(t, err)
	require.Len(t, vis, 4)
	assert.Equal(t, OwnerRepoVisibility{Visibility: "public"}, vis["pub"])
	assert.Equal(t, OwnerRepoVisibility{Visibility: "private"}, vis["priv"])
	assert.Equal(t, OwnerRepoVisibility{Visibility: "private", Archived: true}, vis["old"])
	assert.Equal(t, OwnerRepoVisibility{Visibility: "internal"}, vis["inner"])
}

func TestOwnerRepoVisibilities_NullOwner(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repositoryOwner": nil}})
	})
	_, err := c.OwnerRepoVisibilities(context.Background(), "ghost")
	require.Error(t, err)
}

// TestOrgQueryUntouched locks the identity-critical selections: the shared org
// query must never grow the owner-only fields (its selection set is the cached
// route's byte-locked contract).
func TestOrgQueryUntouched(t *testing.T) {
	assert.NotContains(t, orgDataQuery, "autoMergeRequest")
	assert.NotContains(t, orgDataQuery, "isArchived\n")
	// The repositories-connection totalCount is an owner-query-only extra (the
	// progress hook's "N of M"); the only totalCount the locked query may carry
	// is prFields' reviewRequests one.
	assert.NotContains(t, orgDataQuery, "totalCount\n")
	assert.Contains(t, orgDataQuery, "labels(first: 10)")
	assert.Contains(t, prFields, "labels(first: 10)")
	assert.NotContains(t, prFields, "autoMergeRequest")
}

// TestInstallations_Paginates: a fleet past one page (100) is no longer
// silently truncated.
func TestInstallations_Paginates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		require.Equal(t, "100", r.URL.Query().Get("per_page"))
		out := make([]map[string]any, 0, installationsPerPage)
		switch page {
		case "1":
			for i := 0; i < installationsPerPage; i++ {
				out = append(out, map[string]any{
					"id": i + 1, "account": map[string]any{"login": fmt.Sprintf("owner-%03d", i), "type": "Organization"},
				})
			}
		case "2":
			out = append(out, map[string]any{
				"id": 101, "account": map[string]any{"login": "last-owner", "type": "User"},
			})
		default:
			t.Fatalf("unexpected page %q", page)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	c := testServer(t, mux.ServeHTTP)
	app, err := NewAppAuthenticator("42", pkcs1PEM(testKey(t)), c)
	require.NoError(t, err)

	installs, err := app.Installations(context.Background())
	require.NoError(t, err)
	require.Len(t, installs, 101)
	assert.Equal(t, "last-owner", installs[100].Account.Login)
	assert.True(t, strings.EqualFold("user", installs[100].Account.Type))
}
