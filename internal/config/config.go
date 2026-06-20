package config

import (
	"crypto/rand"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	ListenAddr      string
	DBPath          string
	GitHubToken     string
	WebhookSecret   string
	AllowedOrigins  []string
	RefreshInterval time.Duration

	// Dashboard / OAuth login.
	OAuthClientID     string
	OAuthClientSecret string
	SessionSecret     []byte          // HMAC key for session cookies
	AdminLogins       map[string]bool // lowercased logins granted the all-scopes view
	BaseURL           string          // public base URL (for OAuth redirect_uri); derived from request if empty
}

func Load() Config {
	c := Config{
		ListenAddr:      envOr("LISTEN_ADDR", ":8080"),
		DBPath:          envOr("DB_PATH", "github-mirror.db"),
		GitHubToken:     os.Getenv("GITHUB_TOKEN"),
		WebhookSecret:   os.Getenv("WEBHOOK_SECRET"),
		AllowedOrigins:  parseOrigins(os.Getenv("ALLOWED_ORIGINS")),
		RefreshInterval: 6 * time.Hour,

		OAuthClientID:     os.Getenv("GITHUB_OAUTH_CLIENT_ID"),
		OAuthClientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
		SessionSecret:     sessionSecret(os.Getenv("SESSION_SECRET")),
		AdminLogins:       parseAdmins(envOr("ADMIN_LOGINS", "PazerOP")),
		BaseURL:           os.Getenv("BASE_URL"),
	}
	return c
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseOrigins splits a comma-separated ALLOWED_ORIGINS value into a list of
// allowed CORS origins. An empty value defaults to ["*"] (allow any origin),
// which is safe because the mirror isolates data by token fingerprint.
func parseOrigins(s string) []string {
	out := make([]string, 0)
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"*"}
	}
	return out
}

// parseAdmins builds the set of admin logins (lowercased for case-insensitive
// matching) from a comma-separated list.
func parseAdmins(s string) map[string]bool {
	out := make(map[string]bool)
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[strings.ToLower(p)] = true
		}
	}
	return out
}

// sessionSecret returns the HMAC key for session cookies. When SESSION_SECRET is
// set it is used verbatim; otherwise a random per-process key is generated, which
// means existing sessions are invalidated on restart (acceptable for a cache).
func sessionSecret(env string) []byte {
	if env != "" {
		return []byte(env)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is fatal-ish; fall back to a fixed (insecure) key
		// rather than crash. Operators should set SESSION_SECRET in production.
		slog.Error("could not generate session secret; set SESSION_SECRET", "error", err)
		return []byte("insecure-fallback-session-key")
	}
	slog.Warn("SESSION_SECRET not set; using a random per-process key (sessions reset on restart)")
	return b
}
