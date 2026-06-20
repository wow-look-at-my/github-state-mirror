package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

func testAuthSvc() *auth.Service {
	return auth.New(auth.Config{SessionKey: []byte("test-session-key")})
}

// TestPassthrough_ForwardsUnknownPath verifies a request the mirror does not
// cache is proxied to the upstream GitHub API and the response copied back,
// authenticated with the caller's own token.
func TestPassthrough_ForwardsUnknownPath(t *testing.T) {
	var gotAuth, gotPath, gotQuery string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "testuser"})
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-Custom", "upstream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"number":5}`))
	})
	router, _, _ := newTestStackUpstream(t, testAuthSvc(), upstream)

	req := authedReq("GET", "/repos/o/r/pulls/5?state=open", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"number":5}`, w.Body.String())
	assert.Equal(t, "upstream", w.Header().Get("X-Custom"), "upstream response headers are copied")
	assert.Equal(t, "/repos/o/r/pulls/5", gotPath)
	assert.Equal(t, "state=open", gotQuery, "query string is forwarded")
	assert.Equal(t, "Bearer test-token", gotAuth, "the caller's token is forwarded upstream")
}

// TestPassthrough_PreservesMethodBodyAndStatus verifies writes proxy correctly:
// method, body, and the upstream status code are all preserved.
func TestPassthrough_PreservesMethodBodyAndStatus(t *testing.T) {
	var gotMethod, gotBody string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "testuser"})
			return
		}
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})
	router, _, _ := newTestStackUpstream(t, testAuthSvc(), upstream)

	req := authedReq("POST", "/repos/o/r/pulls", strings.NewReader(`{"title":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "POST", gotMethod)
	assert.Equal(t, `{"title":"hi"}`, gotBody)
	assert.JSONEq(t, `{"created":true}`, w.Body.String())
}

// TestModeB_AppIdentityPartition verifies that a caller asserting an App JWT in
// X-Mirror-Identity is partitioned by the verified app (not the token), and that
// the credential is NOT validated via /user — so a rotating installation token,
// which cannot call /user, works.
func TestModeB_AppIdentityPartition(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app":
			assert.Equal(t, "Bearer app-jwt", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 99, "slug": "pr-minder"})
		case "/user":
			// An installation token 403s here; if Mode B wrongly called it the
			// request would fail. It must not be reached.
			t.Error("/user must not be called in identity mode")
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected upstream call: %s", r.URL.Path)
		}
	})
	router, store, _ := newTestStackUpstream(t, testAuthSvc(), upstream)

	// Seed a cached endpoint in the app's bucket.
	appCtx := actor.WithActor(context.Background(), "app:99")
	require.NoError(t, store.SetPRFiles(appCtx, "o", "r", 7, []dbgen.PrFile{
		{Owner: "o", Repo: "r", PrNumber: 7, Path: "main.go", Additions: 3, Deletions: 1},
	}))

	req := httptest.NewRequest("GET", "/repos/o/r/pulls/7/files", nil)
	req.Header.Set("Authorization", "Bearer install-token-xyz")
	req.Header.Set("X-Mirror-Identity", "app-jwt")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, 1, len(body), "the app bucket's cached file is served")
	assert.Equal(t, "main.go", body[0]["filename"])
}

// TestModeB_ForwardsInstallToken verifies the passthrough still forwards the
// caller's installation token upstream (the identity JWT is only for
// partitioning, never sent upstream).
func TestModeB_ForwardsInstallToken(t *testing.T) {
	var gotAuth, gotIdentity string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 99, "slug": "pr-minder"})
		default:
			gotAuth = r.Header.Get("Authorization")
			gotIdentity = r.Header.Get("X-Mirror-Identity")
			_, _ = w.Write([]byte(`[]`))
		}
	})
	router, _, _ := newTestStackUpstream(t, testAuthSvc(), upstream)

	req := httptest.NewRequest("GET", "/repos/o/r/branches", nil)
	req.Header.Set("Authorization", "Bearer install-token-xyz")
	req.Header.Set("X-Mirror-Identity", "app-jwt")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "Bearer install-token-xyz", gotAuth, "the install token is forwarded upstream")
	assert.Empty(t, gotIdentity, "the identity header must not leak upstream")
}

// TestModeB_InvalidIdentityRejected verifies a forged/expired App JWT (GitHub
// rejects it) yields 401, not a silent fallthrough.
func TestModeB_InvalidIdentityRejected(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"A JSON web token could not be decoded"}`))
			return
		}
		t.Errorf("upstream must not be reached on invalid identity: %s", r.URL.Path)
	})
	router, _, _ := newTestStackUpstream(t, testAuthSvc(), upstream)

	req := httptest.NewRequest("GET", "/repos/o/r/branches", nil)
	req.Header.Set("Authorization", "Bearer install-token-xyz")
	req.Header.Set("X-Mirror-Identity", "forged")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestVerifyAppIdentity_Caches verifies the App identity is fetched once per JWT.
func TestVerifyAppIdentity_Caches(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/app", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 42, "slug": "pr-minder"})
	}))
	t.Cleanup(srv.Close)
	c := ghclient.NewWithBaseURL(srv.URL)

	id, err := c.VerifyAppIdentity(context.Background(), "jwt-1")
	require.NoError(t, err)
	assert.Equal(t, int64(42), id.ID)
	assert.Equal(t, "pr-minder", id.Slug)

	_, err = c.VerifyAppIdentity(context.Background(), "jwt-1")
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "result is cached per JWT")
}
