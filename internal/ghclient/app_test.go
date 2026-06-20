package ghclient

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return key
}

func pkcs1PEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func pkcs8PEM(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// verifyJWT checks an "Authorization: Bearer <jwt>" header: it confirms the
// RS256 signature against pub and returns the decoded claims.
func verifyJWT(t *testing.T, authHeader string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	require.True(t, strings.HasPrefix(authHeader, "Bearer "), "missing Bearer prefix: %q", authHeader)
	token := strings.TrimPrefix(authHeader, "Bearer ")

	parts := strings.Split(token, ".")
	require.Len(t, parts, 3, "JWT must have three segments")

	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(signingInput))
	require.NoError(t, rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig), "JWT signature must verify")

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(claimsJSON, &claims))
	return claims
}

func TestAppAuthenticator_Installations(t *testing.T) {
	key := testKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/app/installations", r.URL.Path)
		claims := verifyJWT(t, r.Header.Get("Authorization"), &key.PublicKey)
		assert.Equal(t, "42", claims["iss"])
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 123, "account": map[string]any{"login": "acme", "type": "Organization"}},
			{"id": 456, "account": map[string]any{"login": "octo", "type": "User"}},
		})
	}))
	t.Cleanup(srv.Close)

	app, err := NewAppAuthenticator("42", pkcs1PEM(key), NewWithBaseURL(srv.URL))
	require.NoError(t, err)

	installs, err := app.Installations(context.Background())
	require.NoError(t, err)
	require.Len(t, installs, 2)
	assert.Equal(t, int64(123), installs[0].ID)
	assert.Equal(t, "acme", installs[0].Account.Login)
	assert.Equal(t, "Organization", installs[0].Account.Type)
	assert.Equal(t, int64(456), installs[1].ID)
}

func TestAppAuthenticator_InstallationToken(t *testing.T) {
	key := testKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/app/installations/123/access_tokens", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		verifyJWT(t, r.Header.Get("Authorization"), &key.PublicKey)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_installationtoken"})
	}))
	t.Cleanup(srv.Close)

	app, err := NewAppAuthenticator("42", pkcs1PEM(key), NewWithBaseURL(srv.URL))
	require.NoError(t, err)

	token, err := app.InstallationToken(context.Background(), 123)
	require.NoError(t, err)
	assert.Equal(t, "ghs_installationtoken", token)
}

func TestAppAuthenticator_InstallationTokenEmpty(t *testing.T) {
	key := testKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": ""})
	}))
	t.Cleanup(srv.Close)

	app, err := NewAppAuthenticator("42", pkcs1PEM(key), NewWithBaseURL(srv.URL))
	require.NoError(t, err)

	_, err = app.InstallationToken(context.Background(), 123)
	assert.Error(t, err)
}

func TestNewAppAuthenticator_PKCS8(t *testing.T) {
	key := testKey(t)
	app, err := NewAppAuthenticator("42", pkcs8PEM(t, key), New())
	require.NoError(t, err)
	assert.NotNil(t, app)
}

func TestNewAppAuthenticator_Errors(t *testing.T) {
	key := testKey(t)

	_, err := NewAppAuthenticator("", pkcs1PEM(key), New())
	assert.Error(t, err, "empty app id must error")

	_, err = NewAppAuthenticator("42", []byte("not a pem"), New())
	assert.Error(t, err, "garbage key must error")
}
