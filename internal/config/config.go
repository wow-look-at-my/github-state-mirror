package config

import (
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
}

func Load() Config {
	c := Config{
		ListenAddr:      envOr("LISTEN_ADDR", ":8080"),
		DBPath:          envOr("DB_PATH", "github-mirror.db"),
		GitHubToken:     os.Getenv("GITHUB_TOKEN"),
		WebhookSecret:   os.Getenv("WEBHOOK_SECRET"),
		AllowedOrigins:  parseOrigins(os.Getenv("ALLOWED_ORIGINS")),
		RefreshInterval: 6 * time.Hour,
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
