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
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "acme", "id": 555})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	gh := ghclient.NewWithBaseURL(srv.URL)
	app, err := ghclient.NewAppAuthenticator("42", testAppKeyPEM(t), gh)
	require.NoError(t, err)

	type recorded struct{ principal, name string }
	var recordedIdentities []recorded
	record := func(_ context.Context, principal, name string) {
		recordedIdentities = append(recordedIdentities, recorded{principal, name})
	}

	sessions, err := AppSessions(app, record)(context.Background())
	require.NoError(t, err)
	require.Len(t, sessions, 1)

	// The session names the installation account -- the owner the periodic
	// refresher will fetch -- instead of discarding it.
	assert.Equal(t, "acme", sessions[0].Owner)
	assert.Equal(t, "Organization", sessions[0].AccountType)
	assert.Equal(t, int64(123), sessions[0].InstallationID)

	// The session is partitioned under the stable installation actor and
	// carries the account login as the principal's display name.
	assert.Equal(t, "app-installation:123", actor.FromContext(sessions[0].Ctx))
	assert.True(t, IsAppInstallationActor(actor.FromContext(sessions[0].Ctx)))
	assert.Equal(t, "acme", actor.NameFromContext(sessions[0].Ctx))

	// The principal->name mapping was recorded (so the dashboard can resolve
	// app-installation:<id> instead of showing "(unknown)").
	assert.Equal(t, []recorded{{"app-installation:123", "acme"}}, recordedIdentities)

	// ...and carries the minted installation token, so API calls made with the
	// session context authenticate as that installation.
	ident, err := gh.ResolveTokenIdentity(sessions[0].Ctx)
	require.NoError(t, err)
	assert.Equal(t, "acme", ident.Login)
	assert.Equal(t, "Bearer ghs_inst123", userAuth)
}

func TestAppSessions_InstallationsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	gh := ghclient.NewWithBaseURL(srv.URL)
	app, err := ghclient.NewAppAuthenticator("42", testAppKeyPEM(t), gh)
	require.NoError(t, err)

	_, err = AppSessions(app, nil)(context.Background())
	assert.Error(t, err)
}
