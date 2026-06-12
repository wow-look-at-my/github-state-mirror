package ghclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithToken_RoundTrip(t *testing.T) {
	ctx := WithToken(context.Background(), "ghp_test123")
	assert.Equal(t, "ghp_test123", tokenFromContext(ctx))
}

func TestTokenFromContext_Empty(t *testing.T) {
	assert.Equal(t, "", tokenFromContext(context.Background()))
}

func testServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewWithBaseURL("", srv.URL)
}

func TestResolveActor_NoToken(t *testing.T) {
	c := New("")
	login, err := c.ResolveActor(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "", login)
}

func TestResolveActor_FromContext(t *testing.T) {
	callCount := 0
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		assert.Equal(t, "/user", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		json.NewEncoder(w).Encode(userResp{Login: "octocat"})
	})

	ctx := WithToken(context.Background(), "test-token")

	login, err := c.ResolveActor(ctx)
	require.NoError(t, err)
	assert.Equal(t, "octocat", login)
	assert.Equal(t, 1, callCount)

	// Second call should use cache.
	login, err = c.ResolveActor(ctx)
	require.NoError(t, err)
	assert.Equal(t, "octocat", login)
	assert.Equal(t, 1, callCount) // no additional API call
}

func TestResolveActor_DefaultToken(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer default-tok", r.Header.Get("Authorization"))
		json.NewEncoder(w).Encode(userResp{Login: "defaultuser"})
	})
	c.defaultToken = "default-tok"

	login, err := c.ResolveActor(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "defaultuser", login)
}

func TestResolveActor_APIError(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	})

	ctx := WithToken(context.Background(), "bad-token")
	_, err := c.ResolveActor(ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestGetAuthenticatedUser(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(userResp{Login: "octocat", AvatarURL: "http://avatar", HTMLURL: "http://url"})
	})

	user, err := c.GetAuthenticatedUser(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "octocat", user.Login)
	assert.Equal(t, "http://avatar", user.AvatarUrl)
}

func TestGetUserOrgs(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]orgResp{
			{Login: "myorg", AvatarURL: "http://avatar", URL: "http://url"},
		})
	})

	orgs, err := c.GetUserOrgs(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, len(orgs))
	assert.Equal(t, "myorg", orgs[0].Login)
}

func TestCompareBranches(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/org/repo/compare/main...feature", r.URL.Path)
		json.NewEncoder(w).Encode(compareResp{AheadBy: 3, BehindBy: 1})
	})

	comp, err := c.CompareBranches(context.Background(), "org", "repo", "main", "feature")
	require.NoError(t, err)
	assert.Equal(t, int64(3), comp.AheadBy)
	assert.Equal(t, int64(1), comp.BehindBy)
}

func TestGetPRFiles(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/org/repo/pulls/42/files", r.URL.Path)
		json.NewEncoder(w).Encode([]prFileResp{
			{Filename: "main.go", Additions: 10, Deletions: 2},
		})
	})

	files, err := c.GetPRFiles(context.Background(), "org", "repo", 42)
	require.NoError(t, err)
	require.Equal(t, 1, len(files))
	assert.Equal(t, "main.go", files[0].Path)
	assert.Equal(t, int64(10), files[0].Additions)
}

func TestDoJSON_SetsContentType(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Write([]byte("{}"))
	})

	err := c.doJSON(context.Background(), "GET", "/test", nil, nil)
	require.NoError(t, err)
}
