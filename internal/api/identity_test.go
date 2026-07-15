package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

func identityAuthSvc() *auth.Service {
	return auth.New(auth.Config{SessionKey: []byte("test-session-key")})
}

const orgReposQuery = `{"query":"{ organization(login: \"my-org\") { repositories { nodes { name } } } }","variables":{"org":"my-org"}}`

// TestModeB_AppIdentityPartition verifies that a caller asserting an App JWT in
// X-Mirror-Identity resolves to the verified app principal (app:<id>), not the
// token, and that the credential is NOT validated via /user — so a rotating
// installation token (which cannot call /user) works. It uses /graphql, the only
// cached route: the app principal's grants reveal the private repo to it.
func TestModeB_AppIdentityPartition(t *testing.T) {
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app":
			assert.Equal(t, "Bearer app-jwt", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 99, "slug": "pr-minder"})
		case "/user":
			// An installation token 403s here; if identity mode wrongly called it
			// the request would fail. It must not be reached.
			t.Error("/user must not be called in identity mode")
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected upstream call: %s", r.URL.Path)
		}
	})
	router, store, _, _ := newTestStackWithGitHub(t, identityAuthSvc(), gh)

	// Seed a private repo into global truth and grant it to the app principal
	// (app:99) -- the identity the X-Mirror-Identity JWT must resolve to.
	ctx := context.Background()
	require.NoError(t, store.UpsertRepo(ctx, dbgen.Repo{
		Owner: "my-org", Name: "repo1", NameWithOwner: "my-org/repo1", Url: "u",
		Visibility: ghdata.VisibilityPrivate,
	}))
	require.NoError(t, store.RecordGrant(ctx, "app:99", "my-org", "repo1", ghdata.GrantSourceListSync, time.Now()))

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(orgReposQuery))
	req.Header.Set("Authorization", "Bearer install-token-xyz")
	req.Header.Set("X-Mirror-Identity", "app-jwt")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	nodes := resp["data"].(map[string]interface{})["organization"].(map[string]interface{})["repositories"].(map[string]interface{})["nodes"].([]interface{})
	require.Equal(t, 1, len(nodes), "served from cache via the app:99 principal's grant")
	assert.Equal(t, "repo1", nodes[0].(map[string]interface{})["name"])
}

// TestModeB_InvalidIdentityRejected verifies a forged/expired App JWT (GitHub
// rejects it at GET /app) yields 401 on a cached route, not a silent fallthrough.
func TestModeB_InvalidIdentityRejected(t *testing.T) {
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"A JSON web token could not be decoded"}`))
			return
		}
		t.Errorf("upstream must not be reached on invalid identity: %s", r.URL.Path)
	})
	router, _, _, _ := newTestStackWithGitHub(t, identityAuthSvc(), gh)

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(orgReposQuery))
	req.Header.Set("Authorization", "Bearer install-token-xyz")
	req.Header.Set("X-Mirror-Identity", "forged")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestModeB_AppIdentityName: a VERIFIED X-Mirror-Identity caller's slug is
// recorded in actor_identities and rides the request log as actor_name — the
// dashboard shows "pr-minder" instead of a bare app:<id>.
func TestModeB_AppIdentityName(t *testing.T) {
	svc := configuredAuth(t) // admin session support for /api/requests
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 99, "slug": "pr-minder"})
			return
		}
		t.Errorf("unexpected upstream call: %s", r.URL.Path)
	})
	router, store, _, _ := newTestStackWithGitHub(t, svc, gh)

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(orgReposQuery))
	req.Header.Set("Authorization", "Bearer install-token-xyz")
	req.Header.Set("X-Mirror-Identity", "app-jwt")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// requireAuth recorded the verified slug for the app principal.
	ids, err := store.ListActorIdentities(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 1)
	assert.Equal(t, "app:99", ids[0].Actor)
	assert.Equal(t, "pr-minder", ids[0].Login)

	// ...and the request-log row carries the name next to the key.
	rl := httptest.NewRequest("GET", "/api/requests", nil)
	rl.AddCookie(mintSession(t, svc, "PazerOP"))
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, rl)
	require.Equal(t, http.StatusOK, rw.Code)
	var snap requestLogSnapshot
	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &snap))
	var found bool
	for _, e := range snap.Recent {
		if e.Path == "/graphql" {
			found = true
			assert.Equal(t, "app:99", e.Actor)
			assert.Equal(t, "pr-minder", e.ActorName)
		}
	}
	require.True(t, found, "the graphql request must be in the log")
}
