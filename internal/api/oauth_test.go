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

// TestOAuthDeviceCode_RelaysToGitHubWithCORS verifies the device-code relay:
// it forwards the SPA's JSON body to github.com's device authorization
// endpoint byte-identical with no bearer token, returns GitHub's device-code
// response verbatim, and carries exactly one Access-Control-Allow-Origin (the
// mirror's) so a browser can start the device flow.
func TestOAuthDeviceCode_RelaysToGitHubWithCORS(t *testing.T) {
	deviceJSON := `{"device_code":"3584d83530557fdd1f46af8289938c8ef79f9dc5","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`
	var gotBody, gotCT, gotAccept, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// GitHub returns its own CORS header; the relay must not duplicate it.
		w.Header().Set("Access-Control-Allow-Origin", "https://github.com")
		_, _ = io.WriteString(w, deviceJSON)
	}))
	defer upstream.Close()

	old := githubDeviceCodeURL
	githubDeviceCodeURL = upstream.URL + "/login/device/code"
	t.Cleanup(func() { githubDeviceCodeURL = old })

	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	// Exactly the request the repo-nightmare SPA sends to start a sign-in.
	spaBody := `{"client_id":"Iv23li8HyDkChFa2ND0B","scope":"read:org repo"}`
	req := httptest.NewRequest(http.MethodPost, "/login/device/code", strings.NewReader(spaBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://sites.pazer.build")
	// Deliberately no Authorization header: the relay must work without one.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, deviceJSON, w.Body.String(), "GitHub's device-code JSON is passed through verbatim")

	// Upstream received the SPA's request unchanged (and no bearer token).
	assert.Equal(t, spaBody, gotBody)
	assert.Equal(t, "application/json", gotCT)
	assert.Equal(t, "application/json", gotAccept)
	assert.Empty(t, gotAuth, "relay must not attach a bearer token")

	// Exactly one Access-Control-Allow-Origin (the mirror's), not GitHub's too.
	acao := w.Header().Values("Access-Control-Allow-Origin")
	require.Len(t, acao, 1, "must not duplicate Access-Control-Allow-Origin")
	assert.Equal(t, "*", acao[0])
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
}

// TestOAuthDeviceCode_Preflight verifies a CORS preflight to the device-code
// relay is answered (204 + ACAO) without reaching GitHub, so the browser
// proceeds.
func TestOAuthDeviceCode_Preflight(t *testing.T) {
	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodOptions, "/login/device/code", nil)
	req.Header.Set("Origin", "https://sites.pazer.build")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

// TestOAuthAccessToken_DeviceGrantPassthrough pins the device flow's polling
// leg on the EXISTING access-token relay: the JSON device_code grant body must
// reach GitHub byte-identical (the relay filters nothing by grant type or
// content type) and GitHub's "authorization_pending" answer must come back
// verbatim with GitHub's status.
func TestOAuthAccessToken_DeviceGrantPassthrough(t *testing.T) {
	pendingJSON := `{"error":"authorization_pending","error_description":"The authorization request is still pending.","error_uri":"https://docs.github.com/developers/apps/authorizing-oauth-apps#error-codes-for-the-device-flow"}`
	var gotBody, gotCT, gotAccept string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// GitHub answers a still-pending device grant with HTTP 200 + an error
		// JSON body (not an RFC 6749 400); the relay passes it through as-is.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, pendingJSON)
	}))
	defer upstream.Close()

	old := githubOAuthTokenURL
	githubOAuthTokenURL = upstream.URL + "/login/oauth/access_token"
	t.Cleanup(func() { githubOAuthTokenURL = old })

	router, _, _, _ := newTestStackWithGitHub(t, testAuth(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	// Exactly the poll the repo-nightmare SPA repeats until the user approves.
	poll := `{"client_id":"Iv23li8HyDkChFa2ND0B","device_code":"3584d83530557fdd1f46af8289938c8ef79f9dc5","grant_type":"urn:ietf:params:oauth:grant-type:device_code"}`
	req := httptest.NewRequest(http.MethodPost, "/login/oauth/access_token", strings.NewReader(poll))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://sites.pazer.build")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, pendingJSON, w.Body.String(), "GitHub's pending answer is passed through verbatim")

	// Upstream received the poll unchanged — no grant-type filtering.
	assert.Equal(t, poll, gotBody)
	assert.Equal(t, "application/json", gotCT)
	assert.Equal(t, "application/json", gotAccept)
}
