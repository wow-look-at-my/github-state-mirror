package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/auth"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

func identityAuthSvc() *auth.Service {
	return auth.New(auth.Config{SessionKey: []byte("test-session-key")})
}

// TestModeB_AppIdentityPartition verifies that a caller asserting an App JWT in
// X-Mirror-Identity is partitioned by the verified app (app:<id>), not the
// token, and that the credential is NOT validated via /user — so a rotating
// installation token (which cannot call /user) works on cached endpoints.
func TestModeB_AppIdentityPartition(t *testing.T) {
	gh := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app":
			assert.Equal(t, "Bearer app-jwt", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 99, "slug": "pr-minder"})
		case "/user":
			// An installation token 403s here; if identity mode wrongly called
			// it the request would fail. It must not be reached.
			t.Error("/user must not be called in identity mode")
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected upstream call: %s", r.URL.Path)
		}
	})
	router, store, _, _ := newTestStackWithGitHub(t, identityAuthSvc(), gh)

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

// TestModeB_InvalidIdentityRejected verifies a forged/expired App JWT (GitHub
// rejects it at GET /app) yields 401 on a cached endpoint, not a silent
// fallthrough to fingerprint mode.
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

	req := httptest.NewRequest("GET", "/user", nil)
	req.Header.Set("Authorization", "Bearer install-token-xyz")
	req.Header.Set("X-Mirror-Identity", "forged")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
