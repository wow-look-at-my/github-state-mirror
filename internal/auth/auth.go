// Package auth implements GitHub OAuth login and signed-cookie sessions for the
// web dashboard. It is deliberately self-contained (stdlib only) and knows
// nothing about the cache: it answers "who is this browser?" and "is this login
// an admin?", and leaves all cache access to the caller.
//
// The dashboard's authorization model is distinct from the data API's. The data
// API isolates by an opaque token fingerprint (see internal/api/router.go); the
// dashboard authenticates a human via GitHub OAuth and authorizes by login. The
// session never grants access to another credential's cached rows — it only lets
// the dashboard decide which cache scopes belong to the logged-in user.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// SessionCookie holds the signed login session.
	SessionCookie = "gsm_session"
	// StateCookie holds the OAuth CSRF state during the login round-trip.
	StateCookie = "gsm_oauth_state"

	sessionTTL = 7 * 24 * time.Hour
)

// Config configures the auth Service. The GitHub endpoint URLs are overridable
// so tests can point them at a local stub; production uses the defaults.
type Config struct {
	ClientID     string
	ClientSecret string
	SessionKey   []byte          // HMAC key for signing session cookies
	AdminLogins  map[string]bool // logins granted the all-scopes admin view

	// Endpoints (defaults applied in New when empty).
	AuthorizeURL string // GitHub OAuth authorize endpoint
	TokenURL     string // GitHub OAuth access-token endpoint
	APIBaseURL   string // GitHub REST API base (for GET /user)

	HTTPClient *http.Client
}

// Service performs the OAuth handshake and signs/verifies session cookies.
type Service struct {
	cfg    Config
	client *http.Client
}

// New builds a Service, applying default GitHub endpoints and HTTP client.
func New(cfg Config) *Service {
	if cfg.AuthorizeURL == "" {
		cfg.AuthorizeURL = "https://github.com/login/oauth/authorize"
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = "https://github.com/login/oauth/access_token"
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "https://api.github.com"
	}
	if cfg.AdminLogins == nil {
		cfg.AdminLogins = map[string]bool{}
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Service{cfg: cfg, client: client}
}

// Configured reports whether OAuth credentials are present. When false the
// dashboard still renders but the login button is disabled.
func (s *Service) Configured() bool {
	return s.cfg.ClientID != "" && s.cfg.ClientSecret != ""
}

// IsAdmin reports whether the login may view all cache scopes. Matching is
// case-insensitive; AdminLogins is expected to hold lowercased logins.
func (s *Service) IsAdmin(login string) bool {
	return s.cfg.AdminLogins[strings.ToLower(login)]
}

// AuthCodeURL builds the GitHub authorize URL for the given redirect URI and
// CSRF state. The "read:user" scope is the minimum that lets us read the login.
func (s *Service) AuthCodeURL(redirectURI, state string) string {
	q := url.Values{}
	q.Set("client_id", s.cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "read:user")
	q.Set("state", state)
	q.Set("allow_signup", "false")
	return s.cfg.AuthorizeURL + "?" + q.Encode()
}

// Exchange swaps an OAuth authorization code for a user access token.
func (s *Service) Exchange(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{}
	form.Set("client_id", s.cfg.ClientID)
	form.Set("client_secret", s.cfg.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("oauth token exchange: %d %s", resp.StatusCode, string(body))
	}

	var out struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", fmt.Errorf("oauth token exchange: %s: %s", out.Error, out.ErrorDescription)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("oauth token exchange: empty access token")
	}
	return out.AccessToken, nil
}

// FetchLogin resolves the GitHub login (and avatar) for an access token.
func (s *Service) FetchLogin(ctx context.Context, token string) (login, avatarURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.APIBaseURL+"/user", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("github GET /user: %d %s", resp.StatusCode, string(body))
	}

	var out struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if out.Login == "" {
		return "", "", fmt.Errorf("github GET /user: empty login")
	}
	return out.Login, out.AvatarURL, nil
}

// ---- Sessions (stateless, HMAC-signed cookies) ----

type sessionPayload struct {
	Login string `json:"login"`
	Exp   int64  `json:"exp"`
}

// SetSession writes a signed session cookie identifying the login.
func (s *Service) SetSession(w http.ResponseWriter, login string, secure bool) {
	p := sessionPayload{Login: login, Exp: time.Now().Add(sessionTTL).Unix()}
	raw, _ := json.Marshal(p)
	body := base64.RawURLEncoding.EncodeToString(raw)
	value := body + "." + s.sign(body)
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// ClearSession expires the session cookie.
func (s *Service) ClearSession(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

// Session returns the logged-in login from the request's signed cookie, or
// ("", false) if absent, malformed, tampered, or expired.
func (s *Service) Session(r *http.Request) (string, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	body, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(s.sign(body))) != 1 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", false
	}
	var p sessionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", false
	}
	if p.Login == "" || time.Now().Unix() > p.Exp {
		return "", false
	}
	return p.Login, true
}

func (s *Service) sign(body string) string {
	mac := hmac.New(sha256.New, s.cfg.SessionKey)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// RandomState returns a URL-safe random string for use as an OAuth CSRF state.
func RandomState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
