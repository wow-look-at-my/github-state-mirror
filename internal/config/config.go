package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	ListenAddr      string
	DBPath          string
	WebhookSecret   string
	AllowedOrigins  []string
	RefreshInterval time.Duration

	// GitHub App credentials for background (periodic) refreshes. The service
	// holds no static user token: API requests are authenticated by the
	// caller's own Authorization header, and the only credential the service
	// itself uses is this GitHub App (signed in per-installation).
	GitHubAppID             string
	GitHubAppPrivateKey     string // inline PEM (literal or \n-escaped)
	GitHubAppPrivateKeyPath string // path to a PEM file (takes precedence)
}

func Load() Config {
	c := Config{
		ListenAddr:              envOr("LISTEN_ADDR", ":8080"),
		DBPath:                  envOr("DB_PATH", "github-mirror.db"),
		WebhookSecret:           os.Getenv("WEBHOOK_SECRET"),
		AllowedOrigins:          parseOrigins(os.Getenv("ALLOWED_ORIGINS")),
		RefreshInterval:         6 * time.Hour,
		GitHubAppID:             os.Getenv("GITHUB_APP_ID"),
		GitHubAppPrivateKey:     os.Getenv("GITHUB_APP_PRIVATE_KEY"),
		GitHubAppPrivateKeyPath: os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"),
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
