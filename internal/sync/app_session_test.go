package sync

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

func testAppKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestAppSessions(t *testing.T) {
	var userAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 123, "account": map[string]any{"login": "acme", "type": "Organization"}},
		})
	})
	mux.HandleFunc("/app/installations/123/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_inst123"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		userAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "acme"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	gh := ghclient.NewWithBaseURL(srv.URL)
	app, err := ghclient.NewAppAuthenticator("42", testAppKeyPEM(t), gh)
	require.NoError(t, err)

	sessions, err := AppSessions(app)(context.Background())
	require.NoError(t, err)
	require.Len(t, sessions, 1)

	// The session is partitioned under the stable installation actor.
	assert.Equal(t, "app-installation:123", actor.FromContext(sessions[0].Ctx))

	// It carries the org the installation covers, so the refresher can populate
	// that org's repos into the app-installation partition.
	assert.Equal(t, []string{"acme"}, sessions[0].Orgs)

	// ...and carries the minted installation token, so API calls made with the
	// session context authenticate as that installation.
	login, err := gh.ResolveActor(sessions[0].Ctx)
	require.NoError(t, err)
	assert.Equal(t, "acme", login)
	assert.Equal(t, "Bearer ghs_inst123", userAuth)
}

func TestAppSessions_UserInstallationSeedsNoOrg(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 7, "account": map[string]any{"login": "alice", "type": "User"}},
		})
	})
	mux.HandleFunc("/app/installations/7/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_inst7"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	gh := ghclient.NewWithBaseURL(srv.URL)
	app, err := ghclient.NewAppAuthenticator("42", testAppKeyPEM(t), gh)
	require.NoError(t, err)

	sessions, err := AppSessions(app)(context.Background())
	require.NoError(t, err)
	require.Len(t, sessions, 1)

	// A User-account installation still yields a session (its token may still be
	// used elsewhere) but seeds no org-repos: the cached query is org-scoped.
	assert.Equal(t, "app-installation:7", actor.FromContext(sessions[0].Ctx))
	assert.Empty(t, sessions[0].Orgs)
}

func TestAppSessions_InstallationsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	gh := ghclient.NewWithBaseURL(srv.URL)
	app, err := ghclient.NewAppAuthenticator("42", testAppKeyPEM(t), gh)
	require.NoError(t, err)

	_, err = AppSessions(app)(context.Background())
	assert.Error(t, err)
}
