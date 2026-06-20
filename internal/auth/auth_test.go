package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T) (*Service, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		if r.Form.Get("code") == "bad" {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code", "error_description": "nope"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "gho_" + r.Form.Get("code")})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_good" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"login": "octocat", "avatar_url": "https://avatars/x"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	svc := New(Config{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		SessionKey:   []byte("test-key"),
		AdminLogins:  map[string]bool{"pazerop": true},
		TokenURL:     srv.URL + "/login/oauth/access_token",
		APIBaseURL:   srv.URL,
	})
	return svc, srv
}

func TestConfigured(t *testing.T) {
	assert.False(t, New(Config{}).Configured())
	assert.False(t, New(Config{ClientID: "x"}).Configured())
	assert.True(t, New(Config{ClientID: "x", ClientSecret: "y"}).Configured())
}

func TestIsAdmin_CaseInsensitive(t *testing.T) {
	svc := New(Config{AdminLogins: map[string]bool{"pazerop": true}})
	assert.True(t, svc.IsAdmin("PazerOP"))
	assert.True(t, svc.IsAdmin("pazerop"))
	assert.False(t, svc.IsAdmin("octocat"))
}

func TestAuthCodeURL(t *testing.T) {
	svc := New(Config{ClientID: "abc", ClientSecret: "s"})
	u := svc.AuthCodeURL("https://example.com/auth/callback", "state123")
	parsed, err := url.Parse(u)
	require.NoError(t, err)
	assert.Equal(t, "github.com", parsed.Host)
	q := parsed.Query()
	assert.Equal(t, "abc", q.Get("client_id"))
	assert.Equal(t, "https://example.com/auth/callback", q.Get("redirect_uri"))
	assert.Equal(t, "state123", q.Get("state"))
	assert.Equal(t, "read:user", q.Get("scope"))
}

func TestExchangeAndFetchLogin(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	token, err := svc.Exchange(ctx, "good", "https://x/cb")
	require.NoError(t, err)
	assert.Equal(t, "gho_good", token)

	login, avatar, err := svc.FetchLogin(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, "octocat", login)
	assert.Equal(t, "https://avatars/x", avatar)
}

func TestExchange_Error(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.Exchange(context.Background(), "bad", "https://x/cb")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad_verification_code")
}

func TestFetchLogin_BadToken(t *testing.T) {
	svc, _ := newTestService(t)
	_, _, err := svc.FetchLogin(context.Background(), "gho_wrong")
	require.Error(t, err)
}

func TestSessionRoundTrip(t *testing.T) {
	svc := New(Config{SessionKey: []byte("the-key")})

	rec := httptest.NewRecorder()
	svc.SetSession(rec, "octocat", false)
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, SessionCookie, cookies[0].Name)
	assert.True(t, cookies[0].HttpOnly)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookies[0])
	login, ok := svc.Session(req)
	assert.True(t, ok)
	assert.Equal(t, "octocat", login)
}

func TestSession_NoCookie(t *testing.T) {
	svc := New(Config{SessionKey: []byte("k")})
	_, ok := svc.Session(httptest.NewRequest("GET", "/", nil))
	assert.False(t, ok)
}

func TestSession_Tampered(t *testing.T) {
	svc := New(Config{SessionKey: []byte("k")})
	rec := httptest.NewRecorder()
	svc.SetSession(rec, "octocat", false)
	c := rec.Result().Cookies()[0]

	// Flip the payload but keep the (now-wrong) signature.
	body, sig, _ := strings.Cut(c.Value, ".")
	_ = body
	tampered := &http.Cookie{Name: SessionCookie, Value: "ZXZpbA." + sig}
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(tampered)
	_, ok := svc.Session(req)
	assert.False(t, ok)
}

func TestSession_WrongKey(t *testing.T) {
	signer := New(Config{SessionKey: []byte("key-a")})
	verifier := New(Config{SessionKey: []byte("key-b")})
	rec := httptest.NewRecorder()
	signer.SetSession(rec, "octocat", false)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(rec.Result().Cookies()[0])
	_, ok := verifier.Session(req)
	assert.False(t, ok, "a cookie signed with a different key must not verify")
}

func TestClearSession(t *testing.T) {
	svc := New(Config{SessionKey: []byte("k")})
	rec := httptest.NewRecorder()
	svc.ClearSession(rec, true)
	c := rec.Result().Cookies()[0]
	assert.Equal(t, SessionCookie, c.Name)
	assert.True(t, c.MaxAge < 0)
	assert.True(t, c.Secure)
}

func TestRandomState(t *testing.T) {
	a, err := RandomState()
	require.NoError(t, err)
	b, err := RandomState()
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	assert.NotEmpty(t, a)
}
