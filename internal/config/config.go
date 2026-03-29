package config

import (
	"os"
	"time"
)

type Config struct {
	ListenAddr      string
	DBPath          string
	GitHubToken     string
	WebhookSecret   string
	RefreshInterval time.Duration
}

func Load() Config {
	c := Config{
		ListenAddr:      envOr("LISTEN_ADDR", ":8080"),
		DBPath:          envOr("DB_PATH", "github-mirror.db"),
		GitHubToken:     os.Getenv("GITHUB_TOKEN"),
		WebhookSecret:   os.Getenv("WEBHOOK_SECRET"),
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
