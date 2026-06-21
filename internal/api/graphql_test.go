package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/database"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

// failOrgFetcher always fails, so the org_repos refresh errors.
type failOrgFetcher struct{}

func (failOrgFetcher) Fetch(_ context.Context, _ string, _ string) (freshness.RefreshResult, error) {
	return freshness.RefreshResult{}, errors.New("github api POST /graphql: 502 Bad Gateway")
}

// When the org_repos fetch fails and nothing is cached, the handler must NOT
// return an empty "200 OK" (which is indistinguishable from "no repos"); it must
// surface the upstream error with a non-200 status and the reason in the body.
func TestGraphQL_FetchErrorSurfacesAsError(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	store := ghdata.NewStore(db)
	mgr := freshness.NewManager(freshness.NewStore(db))
	mgr.RegisterFetcher(freshness.Policy{Kind: syncpkg.KindOrgRepos}, failOrgFetcher{})
	h := &handlers{mgr: mgr, store: store, reqlog: newRequestLog()}

	body := `{"variables":{"org":"my-org"},"query":"query { organization(login: \"my-org\") { repositories { nodes { name } } } }"}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body)).WithContext(seedCtx())
	w := httptest.NewRecorder()
	h.graphql(w, req)

	require.Equal(t, http.StatusBadGateway, w.Code)
	var resp struct {
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Errors, 1)
	assert.Contains(t, resp.Errors[0].Message, "502 Bad Gateway")
	assert.Contains(t, resp.Errors[0].Message, "my-org")
	assert.Equal(t, "UPSTREAM_FETCH_FAILED", resp.Errors[0].Type)
}

func TestGraphQL_BasicQuery(t *testing.T) {
	router, store := setupTestRouter(t)
	ctx := seedCtx()

	// Seed repo data.
	store.SetOrgRepos(ctx, "my-org", []dbgen.Repo{
		{
			Owner:         "my-org",
			Name:          "repo1",
			NameWithOwner: "my-org/repo1",
			Url:           "https://github.com/my-org/repo1",
			DefaultBranch: sql.NullString{String: "main", Valid: true},
			OwnerLogin:    sql.NullString{String: "my-org", Valid: true},
			OwnerAvatar:   sql.NullString{String: "https://avatar", Valid: true},
			OwnerUrl:      sql.NullString{String: "https://github.com/my-org", Valid: true},
		},
	})

	// Seed PRs with labels.
	store.SetRepoPRs(ctx, "my-org", "repo1", []dbgen.PullRequest{
		{
			Owner:            "my-org",
			Repo:             "repo1",
			Number:           1,
			Title:            "Test PR",
			Url:              "https://github.com/my-org/repo1/pull/1",
			State:            "OPEN",
			CreatedAt:        "2024-01-01",
			UpdatedAt:        "2024-01-02",
			AuthorLogin:      sql.NullString{String: "dev", Valid: true},
			AuthorAvatar:     sql.NullString{String: "https://avatar/dev", Valid: true},
			AuthorUrl:        sql.NullString{String: "https://github.com/dev", Valid: true},
			HeadRefName:      sql.NullString{String: "feature", Valid: true},
			BaseRefName:      sql.NullString{String: "main", Valid: true},
			HeadRefOid:       sql.NullString{String: "abc123", Valid: true},
			LastCommitStatus: sql.NullString{String: "SUCCESS", Valid: true},
		},
	}, map[int64][]dbgen.PrLabel{
		1: {{Owner: "my-org", Repo: "repo1", PrNumber: 1, Name: "bug", Color: "d73a4a"}},
	})

	body := `{"query":"{ organization(login: \"my-org\") { repositories { nodes { name } } } }","variables":{"org":"my-org"}}`
	req := authedReq(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	data := resp["data"].(map[string]interface{})
	org := data["organization"].(map[string]interface{})
	repos := org["repositories"].(map[string]interface{})

	// pageInfo must be present — clients read pageInfo.hasNextPage unconditionally.
	pageInfo := repos["pageInfo"].(map[string]interface{})
	assert.Equal(t, false, pageInfo["hasNextPage"])

	nodes := repos["nodes"].([]interface{})
	require.Equal(t, 1, len(nodes))

	repoNode := nodes[0].(map[string]interface{})
	assert.Equal(t, "repo1", repoNode["name"])
	assert.Equal(t, "my-org/repo1", repoNode["nameWithOwner"])

	// Check PRs.
	prs := repoNode["pullRequests"].(map[string]interface{})
	prNodes := prs["nodes"].([]interface{})
	require.Equal(t, 1, len(prNodes))

	prNode := prNodes[0].(map[string]interface{})
	assert.Equal(t, float64(1), prNode["number"])
	assert.Equal(t, "Test PR", prNode["title"])
	assert.Equal(t, false, prNode["isDraft"])

	// Check labels.
	labels := prNode["labels"].(map[string]interface{})
	labelNodes := labels["nodes"].([]interface{})
	require.Equal(t, 1, len(labelNodes))
	assert.Equal(t, "bug", labelNodes[0].(map[string]interface{})["name"])

	// Check author.
	author := prNode["author"].(map[string]interface{})
	assert.Equal(t, "dev", author["login"])

	// Check commit status.
	commits := prNode["commits"].(map[string]interface{})
	commitNodes := commits["nodes"].([]interface{})
	require.Equal(t, 1, len(commitNodes))
}

// TestGraphQL_PRWithoutStatus verifies that a PR with no recorded CI status still
// returns a well-formed commits object whose statusCheckRollup is null, rather
// than a null commits. Clients dereference commits.nodes[0].commit unconditionally,
// so a null commits would crash them.
func TestGraphQL_PRWithoutStatus(t *testing.T) {
	router, store := setupTestRouter(t)
	ctx := seedCtx()

	store.SetOrgRepos(ctx, "my-org", []dbgen.Repo{
		{Owner: "my-org", Name: "repo1", NameWithOwner: "my-org/repo1", Url: "u1"},
	})
	store.SetRepoPRs(ctx, "my-org", "repo1", []dbgen.PullRequest{
		{
			Owner:     "my-org",
			Repo:      "repo1",
			Number:    1,
			Title:     "No status PR",
			Url:       "https://github.com/my-org/repo1/pull/1",
			State:     "OPEN",
			CreatedAt: "2024-01-01",
			UpdatedAt: "2024-01-02",
			// LastCommitStatus intentionally left invalid (no CI status recorded).
		},
	}, map[int64][]dbgen.PrLabel{})

	body := `{"query":"{ organization(login: \"my-org\") { repositories { nodes { name } } } }","variables":{"org":"my-org"}}`
	req := authedReq(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	data := resp["data"].(map[string]interface{})
	org := data["organization"].(map[string]interface{})
	repos := org["repositories"].(map[string]interface{})
	nodes := repos["nodes"].([]interface{})
	require.Equal(t, 1, len(nodes))

	prNodes := nodes[0].(map[string]interface{})["pullRequests"].(map[string]interface{})["nodes"].([]interface{})
	require.Equal(t, 1, len(prNodes))

	// commits must be a well-formed object (not null) with one node whose
	// statusCheckRollup is null.
	commits := prNodes[0].(map[string]interface{})["commits"].(map[string]interface{})
	commitNodes := commits["nodes"].([]interface{})
	require.Equal(t, 1, len(commitNodes))
	commit := commitNodes[0].(map[string]interface{})["commit"].(map[string]interface{})
	rollup, present := commit["statusCheckRollup"]
	assert.True(t, present, "statusCheckRollup key should be present")
	assert.Nil(t, rollup, "statusCheckRollup should be null when no CI status is recorded")
}

func TestGraphQL_OrgFromQueryFallback(t *testing.T) {
	router, store := setupTestRouter(t)
	ctx := seedCtx()

	store.SetOrgRepos(ctx, "fallback-org", []dbgen.Repo{
		{Owner: "fallback-org", Name: "repo1", NameWithOwner: "fallback-org/repo1", Url: "u1"},
	})

	// No "org" in variables — should extract from query string.
	body := `{"query":"{ organization(login: \"fallback-org\") { repositories { nodes { name } } } }","variables":{}}`
	req := authedReq(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	data := resp["data"].(map[string]interface{})
	org := data["organization"].(map[string]interface{})
	repos := org["repositories"].(map[string]interface{})
	nodes := repos["nodes"].([]interface{})
	assert.Equal(t, 1, len(nodes))
}

// TestGraphQL_NonCachedQueryForwarded verifies that a GraphQL query the mirror
// cannot answer from cache (here, a viewer query with no org/repositories) is
// forwarded to GitHub uncached, with the caller's body and token, rather than
// rejected.
func TestGraphQL_NonCachedQueryForwarded(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" { // requireAuth's token validation
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "testuser"})
			return
		}
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"}}}`))
	})
	router, _, _, _ := newTestStackWithGitHub(t, auth.New(auth.Config{SessionKey: []byte("k")}), gh)

	body := `{"query":"{ viewer { login } }","variables":{}}`
	req := authedReq(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/graphql", gotPath, "should forward to GitHub's /graphql")
	assert.Equal(t, "Bearer "+testToken, gotAuth, "should forward the caller's token")
	assert.Equal(t, body, gotBody, "should forward the original request body")

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	viewer := resp["data"].(map[string]interface{})["viewer"].(map[string]interface{})
	assert.Equal(t, "octocat", viewer["login"])
}

func TestGraphQL_BadJSON(t *testing.T) {
	router, _ := setupTestRouter(t)

	req := authedReq(http.MethodPost, "/graphql", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGraphQL_EmptyRepos(t *testing.T) {
	router, _ := setupTestRouter(t)

	body := `{"query":"{ organization(login: \"empty-org\") { repositories { nodes { name } } } }","variables":{"org":"empty-org"}}`
	req := authedReq(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	data := resp["data"].(map[string]interface{})
	org := data["organization"].(map[string]interface{})
	repos := org["repositories"].(map[string]interface{})
	nodes := repos["nodes"].([]interface{})
	assert.Equal(t, 0, len(nodes))
}

func TestExtractOrgFromQuery(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{`{ organization(login: "my-org") { repos } }`, "my-org"},
		{`query { organization(login: "test-org") { name } }`, "test-org"},
		{`{ viewer { login } }`, ""},
		{`organization(login:`, ""},
		{`organization(login: "unclosed`, ""},
		{"", ""},
	}

	for _, tt := range tests {
		got := extractOrgFromQuery(tt.query)
		assert.Equal(t, tt.want, got)
	}
}
