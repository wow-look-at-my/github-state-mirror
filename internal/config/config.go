package config

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	ListenAddr    string
	DBPath        string
	WebhookSecret string
	// SubscriptionsDBPath is the subscriber-notification config DB — a
	// SEPARATE SQLite file that survives the cache DB's SchemaVersion nukes.
	// Empty = derive from DBPath (github-mirror.db ->
	// github-mirror-subscriptions.db; notify.DeriveDBPath, applied in
	// cmd/server).
	SubscriptionsDBPath string
	AllowedOrigins      []string
	RefreshInterval     time.Duration

	// GitHub App credentials for background (periodic) refreshes. The service
	// holds no static user token: API requests are authenticated by the
	// caller's own Authorization header, and the only credential the service
	// itself uses is this GitHub App (signed in per-installation).
	GitHubAppID             string
	GitHubAppPrivateKey     string // inline PEM (literal or \n-escaped)
	GitHubAppPrivateKeyPath string // path to a PEM file (takes precedence)

	// Dashboard / OAuth login.
	OAuthClientID     string
	OAuthClientSecret string
	SessionSecret     []byte          // HMAC key for session cookies
	AdminLogins       map[string]bool // lowercased logins granted the all-scopes view
	BaseURL           string          // public base URL (for OAuth redirect_uri); derived from request if empty
}

func Load() Config {
	c := Config{
		ListenAddr:          envOr("LISTEN_ADDR", ":8080"),
		DBPath:              envOr("DB_PATH", "github-mirror.db"),
		WebhookSecret:       os.Getenv("WEBHOOK_SECRET"),
		SubscriptionsDBPath: os.Getenv("SUBSCRIPTIONS_DB_PATH"),
		AllowedOrigins:      parseOrigins(os.Getenv("ALLOWED_ORIGINS")),
		RefreshInterval:     6 * time.Hour,

		GitHubAppID:             os.Getenv("GITHUB_APP_ID"),
		GitHubAppPrivateKey:     os.Getenv("GITHUB_APP_PRIVATE_KEY"),
		GitHubAppPrivateKeyPath: os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"),

		OAuthClientID:     os.Getenv("GITHUB_OAUTH_CLIENT_ID"),
		OAuthClientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
		SessionSecret:     sessionSecret(os.Getenv("SESSION_SECRET")),
		AdminLogins:       parseAdmins(envOr("ADMIN_LOGINS", "PazerOP")),
		BaseURL:           os.Getenv("BASE_URL"),
	}
	return c
}

// GitHubAppConfigured reports whether a GitHub App ID was provided. The private
// key is validated separately (see AppPrivateKeyPEM) so a half-configured app is
// surfaced as an error rather than silently ignored.
func (c Config) GitHubAppConfigured() bool {
	return c.GitHubAppID != ""
}

// AppPrivateKeyPEM returns the GitHub App private key as PEM bytes, read from
// GITHUB_APP_PRIVATE_KEY_PATH if set, otherwise from the inline
// GITHUB_APP_PRIVATE_KEY value. Inline values may use \n-escaped newlines
// (common when a PEM is stored in a single-line env var). It returns an error
// only when a configured path cannot be read; a wholly unset key returns
// (nil, nil) so the caller can treat the app as not configured.
func (c Config) AppPrivateKeyPEM() ([]byte, error) {
	if c.GitHubAppPrivateKeyPath != "" {
		b, err := os.ReadFile(c.GitHubAppPrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read GITHUB_APP_PRIVATE_KEY_PATH: %w", err)
		}
		return b, nil
	}
	if c.GitHubAppPrivateKey != "" {
		return []byte(unescapeNewlines(c.GitHubAppPrivateKey)), nil
	}
	return nil, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// unescapeNewlines turns a single-line PEM that uses literal "\n" sequences into
// a real multi-line PEM. If the value already contains real newlines it is
// returned unchanged.
func unescapeNewlines(s string) string {
	if strings.Contains(s, "\n") {
		return s
	}
	return strings.ReplaceAll(s, `\n`, "\n")
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
