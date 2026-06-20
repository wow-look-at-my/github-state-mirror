package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOAuthAccessToken_RelaysToGitHubWithCORS verifies the OAuth token-exchange
// relay: it forwards the form body to github.com's token endpoint with no bearer
// token, returns GitHub's response verbatim, and carries exactly one
// Access-Control-Allow-Origin (the mirror's) so a browser can read the token.
func TestOAuthAccessToken_RelaysToGitHubWithCORS(t *testing.T) {
	var gotBody, gotCT, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		// GitHub returns its own CORS header; the relay must not duplicate it.
		w.Header().Set("Access-Control-Allow-Origin", "https://github.com")
		_, _ = io.WriteString(w, "access_token=gho_test123&scope=&token_type=bearer")
	}))
	defer upstream.Close()

	old := githubOAuthTokenURL
	githubOAuthTokenURL = upstream.URL + "/login/oauth/access_token"
	t.Cleanup(func() { githubOAuthTokenURL = old })

	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	form := "client_id=abc&client_secret=shh&code=xyz"
	req := httptest.NewRequest(http.MethodPost, "/login/oauth/access_token", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://sites.pazer.build")
	// Deliberately no Authorization header: the relay must work without one.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "access_token=gho_test123", "GitHub's token response is passed through")

	// Upstream received the exchange unchanged (and no bearer token was added).
	assert.Equal(t, form, gotBody)
	assert.Equal(t, "application/x-www-form-urlencoded", gotCT)
	assert.Empty(t, gotAuth, "relay must not attach a bearer token")

	// Exactly one Access-Control-Allow-Origin (the mirror's), not GitHub's too.
	acao := w.Header().Values("Access-Control-Allow-Origin")
	require.Len(t, acao, 1, "must not duplicate Access-Control-Allow-Origin")
	assert.Equal(t, "*", acao[0])
	assert.Equal(t, "application/x-www-form-urlencoded; charset=utf-8", w.Header().Get("Content-Type"))
}

// TestOAuthAccessToken_Preflight verifies a CORS preflight to the relay is
// answered (204 + ACAO) without reaching GitHub, so the browser proceeds.
func TestOAuthAccessToken_Preflight(t *testing.T) {
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodOptions, "/login/oauth/access_token", nil)
	req.Header.Set("Origin", "https://sites.pazer.build")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}
