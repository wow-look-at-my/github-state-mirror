package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ghPullListQuery is the query text `gh pr list --json number` sends.
const ghPullListQuery = `fragment pr on PullRequest{number}
		query PullRequestList(
			$owner: String!,
			$repo: String!,
			$limit: Int!,
			$endCursor: String,
			$baseBranch: String,
			$headBranch: String,
			$state: [PullRequestState!] = OPEN
		) {
			repository(owner: $owner, name: $repo) {
				pullRequests(
					states: $state,
					baseRefName: $baseBranch,
					headRefName: $headBranch,
					first: $limit,
					after: $endCursor,
					orderBy: {field: CREATED_AT, direction: DESC}
				) {
					totalCount
					nodes {
						...pr
					}
					pageInfo {
						hasNextPage
						endCursor
					}
				}
			}
		}`

func graphqlBody(t *testing.T, query string, vars map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	require.NoError(t, err)
	return b
}

func graphqlStack(t *testing.T, answer func(w http.ResponseWriter, body []byte)) (http.Handler, *int32) {
	t.Helper()
	router, _, _, u := respCacheStack(t)
	var hits int32
	u.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/graphql" {
			return false
		}
		atomic.AddInt32(&hits, 1)
		body, _ := io.ReadAll(r.Body)
		answer(w, body)
		return true
	}
	return router, &hits
}

func postGraphQL(t *testing.T, router http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, router, authedReq("POST", "/graphql", bytes.NewReader(body)))
}

func pullListAnswer(w http.ResponseWriter, _ []byte) {
	writeGitHubJSON(w, map[string]any{"data": map[string]any{"repository": map[string]any{
		"pullRequests": map[string]any{
			"totalCount": 1,
			"nodes":      []any{map[string]any{"number": 7}},
			"pageInfo":   map[string]any{"hasNextPage": false, "endCursor": "Y3Vyc29yOnYyOpHOAAAAAQ=="},
		},
	}}})
}

func TestGraphQLPulls_HitMissIdenticalAndFlushedByPullRequest(t *testing.T) {
	router, hits := graphqlStack(t, pullListAnswer)
	body := graphqlBody(t, ghPullListQuery, map[string]any{"owner": "org1", "repo": "repo1", "limit": 30, "state": []string{"OPEN"}})

	miss := postGraphQL(t, router, body)
	require.Equal(t, http.StatusOK, miss.Code)
	assert.Equal(t, "miss", miss.Header().Get(cacheHeader))
	hit := postGraphQL(t, router, body)
	assert.Equal(t, "hit", hit.Header().Get(cacheHeader))
	assert.Equal(t, miss.Body.String(), hit.Body.String(), "hit and miss must be byte-identical")
	assert.Equal(t, int32(1), atomic.LoadInt32(hits))

	// Another spelling of the same query is the same key.
	reformatted := graphqlBody(t, "# gh\n"+compactGraphQL(ghPullListQuery), map[string]any{"state": []string{"OPEN"}, "limit": 30, "repo": "repo1", "owner": "org1"})
	assert.Equal(t, "hit", postGraphQL(t, router, reformatted).Header().Get(cacheHeader))

	// Different variables are a different question.
	closed := graphqlBody(t, ghPullListQuery, map[string]any{"owner": "org1", "repo": "repo1", "limit": 30, "state": []string{"CLOSED", "MERGED"}})
	assert.Equal(t, "miss", postGraphQL(t, router, closed).Header().Get(cacheHeader))
	assert.Equal(t, int32(2), atomic.LoadInt32(hits))

	postWebhookJSON(t, router, "pull_request", map[string]any{
		"action": "opened", "number": 8, "repository": fixtureRepo(),
		"pull_request": upstreamSinglePR(8, "open", "New", "feat", shaCommit, "2026-07-01T10:05:00Z"),
	})
	assert.Equal(t, "miss", postGraphQL(t, router, body).Header().Get(cacheHeader))
	assert.Equal(t, int32(3), atomic.LoadInt32(hits))
}

// compactGraphQL joins the query onto one line with commas, a spelling the
// tokenizer must read as the same stream.
func compactGraphQL(q string) string {
	var b bytes.Buffer
	for _, f := range bytes.Fields([]byte(q)) {
		b.Write(f)
		b.WriteString(" ,")
	}
	return b.String()
}

func TestGraphQLPulls_ErrorsNeverStored(t *testing.T) {
	router, hits := graphqlStack(t, func(w http.ResponseWriter, _ []byte) {
		writeGitHubJSON(w, map[string]any{
			"data":   map[string]any{"repository": nil},
			"errors": []any{map[string]any{"type": "NOT_FOUND", "message": "Could not resolve to a Repository"}},
		})
	})
	body := graphqlBody(t, ghPullListQuery, map[string]any{"owner": "org1", "repo": "repo1", "limit": 30})
	for i := 0; i < 2; i++ {
		w := postGraphQL(t, router, body)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Header().Get(cacheHeader), "request %d", i)
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(hits))
}

func TestGraphQLPulls_UnmodeledShapesForward(t *testing.T) {
	router, hits := graphqlStack(t, pullListAnswer)
	for i, q := range []string{
		// A field another event moves.
		`query { repository(owner: "org1", name: "repo1") { pullRequests(first: 5) { nodes { number reviews { totalCount } } } } }`,
		// A directive.
		`query { repository(owner: "org1", name: "repo1") @include(if: true) { pullRequests(first: 5) { totalCount } } }`,
		// A second root field.
		`query { viewer { login } repository(owner: "org1", name: "repo1") { pullRequests(first: 5) { totalCount } } }`,
		// An unmodeled argument.
		`query { repository(owner: "org1", name: "repo1") { pullRequests(first: 5, labels: ["x"]) { totalCount } } }`,
		// A mutation.
		`mutation { repository(owner: "org1", name: "repo1") { pullRequests(first: 5) { totalCount } } }`,
	} {
		w := postGraphQL(t, router, graphqlBody(t, q, nil))
		require.Equal(t, http.StatusOK, w.Code, "query %d", i)
		assert.Empty(t, w.Header().Get(cacheHeader), "query %d must forward", i)
	}
	assert.Equal(t, int32(5), atomic.LoadInt32(hits))

	// Literal owner and name are modeled.
	lit := graphqlBody(t, `{ repository(owner: "org1", name: "repo1") { pullRequests(first: 5) { totalCount nodes { number } } } }`, nil)
	assert.Equal(t, "miss", postGraphQL(t, router, lit).Header().Get(cacheHeader))
	assert.Equal(t, "hit", postGraphQL(t, router, lit).Header().Get(cacheHeader))
}
