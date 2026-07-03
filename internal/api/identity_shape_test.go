package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// jsonType returns the JSON kind of a decoded value, for structural comparison.
func jsonType(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// assertSameShape asserts that got has the same structure as want (the key set of
// every object, the JSON type of every leaf, and the element shape of arrays),
// WITHOUT comparing scalar values (logins, SHAs, timestamps, counts vary). This
// is how we hold the cache to "structurally identical to GitHub": want is a real
// GitHub response fixture, got is the mirror's cached response.
func assertSameShape(t *testing.T, path string, want, got interface{}) {
	t.Helper()
	switch w := want.(type) {
	case map[string]interface{}:
		g, ok := got.(map[string]interface{})
		if !assert.Truef(t, ok, "%s: want object, got %s", path, jsonType(got)) {
			return
		}
		for k := range w {
			_, present := g[k]
			assert.Truef(t, present, "%s: cache is missing key %q that GitHub returns", path, k)
		}
		for k := range g {
			_, present := w[k]
			assert.Truef(t, present, "%s: cache returns extra key %q not in GitHub's shape", path, k)
		}
		for k, wv := range w {
			if gv, ok := g[k]; ok {
				assertSameShape(t, path+"."+k, wv, gv)
			}
		}
	case []interface{}:
		g, ok := got.([]interface{})
		if !assert.Truef(t, ok, "%s: want array, got %s", path, jsonType(got)) {
			return
		}
		// Compare element [0] shape when both have one (GraphQL arrays are
		// homogeneous, so one element pins the shape).
		if len(w) > 0 && len(g) > 0 {
			assertSameShape(t, path+"[0]", w[0], g[0])
		}
	default:
		assert.Equalf(t, jsonType(want), jsonType(got), "%s: type mismatch", path)
	}
}

// TestGraphQL_MatchesGitHubShape asserts the cached org-repos GraphQL response is
// structurally identical to GitHub's response for the canonical query — same key
// sets, nesting, and leaf types — by comparing against a recorded fixture. If the
// assembler drops or adds a field relative to GitHub, this fails.
func TestGraphQL_MatchesGitHubShape(t *testing.T) {
	router, store := setupTestRouter(t)

	repos := []dbgen.Repo{{
		Owner:               "my-org",
		Name:                "repo1",
		NameWithOwner:       "my-org/repo1",
		Url:                 "https://github.com/my-org/repo1",
		IsDisabled:          0,
		PushedAt:            sql.NullString{String: "2024-01-01T00:00:00Z", Valid: true},
		DefaultBranch:       sql.NullString{String: "main", Valid: true},
		DefaultBranchStatus: sql.NullString{String: "SUCCESS", Valid: true},
		OwnerLogin:          sql.NullString{String: "my-org", Valid: true},
		OwnerAvatar:         sql.NullString{String: "https://avatars/u", Valid: true},
		OwnerUrl:            sql.NullString{String: "https://github.com/my-org", Valid: true},
	}}
	prs := []dbgen.PullRequest{{
		Owner:              "my-org",
		Repo:               "repo1",
		Number:             1,
		Title:              "Example PR",
		Url:                "https://github.com/my-org/repo1/pull/1",
		State:              "OPEN",
		CreatedAt:          "2024-01-01T00:00:00Z",
		UpdatedAt:          "2024-01-02T00:00:00Z",
		Additions:          sql.NullInt64{Int64: 5, Valid: true},
		Deletions:          sql.NullInt64{Int64: 2, Valid: true},
		Mergeable:          sql.NullString{String: "MERGEABLE", Valid: true},
		AuthorLogin:        sql.NullString{String: "dev", Valid: true},
		AuthorAvatar:       sql.NullString{String: "https://avatars/dev", Valid: true},
		AuthorUrl:          sql.NullString{String: "https://github.com/dev", Valid: true},
		HeadRefName:        sql.NullString{String: "feature", Valid: true},
		BaseRefName:        sql.NullString{String: "main", Valid: true},
		HeadRefOid:         sql.NullString{String: "abc123", Valid: true},
		ReviewRequestCount: sql.NullInt64{Int64: 1, Valid: true},
		LastCommitStatus:   sql.NullString{String: "SUCCESS", Valid: true},
	}}
	now := time.Now()
	require.NoError(t, store.SyncOrgTruth(context.Background(), "my-org", ghdata.OrgSyncData{
		Repos:     repos,
		PRsByRepo: map[string][]dbgen.PullRequest{"my-org/repo1": prs},
		LabelsByPR: map[string]map[int64][]dbgen.PrLabel{
			"my-org/repo1": {1: {{Owner: "my-org", Repo: "repo1", PrNumber: 1, Name: "bug", Color: "d73a4a"}}},
		},
	}, testUserActor, now, now))

	body := `{"query":"{ organization(login: \"my-org\") { repositories { nodes { name } } } }","variables":{"org":"my-org"}}`
	req := authedReq(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var got interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))

	raw, err := os.ReadFile(filepath.Join("testdata", "github", "graphql_org_repos.json"))
	require.NoError(t, err)
	var want interface{}
	require.NoError(t, json.Unmarshal(raw, &want))

	assertSameShape(t, "$", want, got)

	// Enum vocabulary is structural in GraphQL: assert the cache emits values from
	// GitHub's enums, not e.g. a lowercased REST value.
	pr := firstPR(t, got)
	assert.Contains(t, []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}, pr["mergeable"], "mergeable must be a GraphQL enum value")
	rollup := pr["commits"].(map[string]interface{})["nodes"].([]interface{})[0].(map[string]interface{})["commit"].(map[string]interface{})["statusCheckRollup"].(map[string]interface{})
	assert.Contains(t, []string{"SUCCESS", "FAILURE", "PENDING", "ERROR", "EXPECTED"}, rollup["state"], "statusCheckRollup.state must be a GraphQL enum value")
}

func firstPR(t *testing.T, resp interface{}) map[string]interface{} {
	t.Helper()
	repos := resp.(map[string]interface{})["data"].(map[string]interface{})["organization"].(map[string]interface{})["repositories"].(map[string]interface{})["nodes"].([]interface{})
	require.NotEmpty(t, repos)
	prs := repos[0].(map[string]interface{})["pullRequests"].(map[string]interface{})["nodes"].([]interface{})
	require.NotEmpty(t, prs)
	return prs[0].(map[string]interface{})
}
