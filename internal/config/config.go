package config

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/go-containers/set"
)

// defaultCacheMaxRows matches ghdata.CacheMaxRows' own initializer, pinned by TestCacheMaxRowsDefaultMatchesGhdata.
const defaultCacheMaxRows int64 = 1_000_000

// defaultRefreshInterval is the default REFRESH_INTERVAL.
const defaultRefreshInterval = 6 * time.Hour

// defaultReplayInterval is minutes, not hours: the gap before a lost delivery is replayed can stall a PR forever.
const defaultReplayInterval = 5 * time.Minute

// maxPassthroughDebounce must stay in step with internal/api's DebounceMaxWindow (TestDebounceWindowBoundMatchesAPI).
const (
	defaultPassthroughDebounce = 5 * time.Second
	maxPassthroughDebounce     = 30 * time.Second
)

// maxWebhookReorderWindow stays under GitHub's single-digit-second delivery timeout.
const (
	defaultWebhookReorderWindow = 2 * time.Second
	maxWebhookReorderWindow     = 5 * time.Second
)

type Config struct {
	ListenAddr    string
	DBPath        string
	WebhookSecret string
	// SubscriptionsDBPath is a SEPARATE SQLite file; empty derives from DBPath (notify.DeriveDBPath).
	SubscriptionsDBPath string
	AllowedOrigins      []string
	RefreshInterval     time.Duration
	ReplayInterval      time.Duration

	// PassthroughDebounce holds an eligible uncacheable read so concurrent
	PassthroughDebounce time.Duration

	// WebhookReorderWindow holds a delivery so same-subject deliveries sort and apply oldest-first; 0 skips straight to the watermark gate.
	WebhookReorderWindow time.Duration

	// CacheMaxRows is the per-table row ceiling; only git_commits_cache (no TTL) actually grows to it.
	CacheMaxRows int64

	// GitHub App credentials for background refreshes; the service holds no static user token otherwise.
	GitHubAppID             string
	GitHubAppPrivateKey     string // inline PEM (literal or \n-escaped)
	GitHubAppPrivateKeyPath string // path to a PEM file (takes precedence)

	// Dashboard / OAuth login.
	OAuthClientID     string
	OAuthClientSecret string
	SessionSecret     []byte          // HMAC key for session cookies
	AdminLogins       set.Set[string] // lowercased logins granted the all-scopes view
	BaseURL           string          // public base URL (for OAuth redirect_uri); derived from request if empty
}

// Load reads the configuration from the environment. It returns an error only
// for a value that is present but invalid (a loud misconfiguration the server
// must refuse to start on) -- an absent optional value always falls back to
// its default.
func Load() (Config, error) {
	cacheMaxRows, err := parseCacheMaxRows(os.Getenv("CACHE_MAX_ROWS"))
	if err != nil {
		return Config{}, err
	}
	replayInterval, err := parseReplayInterval(os.Getenv("WEBHOOK_REPLAY_INTERVAL"))
	if err != nil {
		return Config{}, err
	}
	refreshInterval, err := parseRefreshInterval(os.Getenv("REFRESH_INTERVAL"))
	if err != nil {
		return Config{}, err
	}
	debounce, err := parsePassthroughDebounce(os.Getenv("PASSTHROUGH_DEBOUNCE"))
	if err != nil {
		return Config{}, err
	}
	reorderWindow, err := parseWebhookReorderWindow(os.Getenv("WEBHOOK_REORDER_WINDOW"))
	if err != nil {
		return Config{}, err
	}
	c := Config{
		ListenAddr:           envOr("LISTEN_ADDR", ":8080"),
		DBPath:               envOr("DB_PATH", "github-mirror.db"),
		WebhookSecret:        os.Getenv("WEBHOOK_SECRET"),
		SubscriptionsDBPath:  os.Getenv("SUBSCRIPTIONS_DB_PATH"),
		AllowedOrigins:       parseOrigins(os.Getenv("ALLOWED_ORIGINS")),
		RefreshInterval:      refreshInterval,
		ReplayInterval:       replayInterval,
		PassthroughDebounce:  debounce,
		WebhookReorderWindow: reorderWindow,
		CacheMaxRows:         cacheMaxRows,

		GitHubAppID:             os.Getenv("GITHUB_APP_ID"),
		GitHubAppPrivateKey:     os.Getenv("GITHUB_APP_PRIVATE_KEY"),
		GitHubAppPrivateKeyPath: os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"),

		OAuthClientID:     os.Getenv("GITHUB_OAUTH_CLIENT_ID"),
		OAuthClientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
		SessionSecret:     sessionSecret(os.Getenv("SESSION_SECRET")),
		AdminLogins:       parseAdmins(envOr("ADMIN_LOGINS", "PazerOP")),
		BaseURL:           os.Getenv("BASE_URL"),
	}
	return c, nil
}

// parseCacheMaxRows parses CACHE_MAX_ROWS; unparseable or < 1 fails startup rather than silently falling back.
func parseCacheMaxRows(s string) (int64, error) {
	if s == "" {
		return defaultCacheMaxRows, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid CACHE_MAX_ROWS %q: %w", s, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("invalid CACHE_MAX_ROWS %d: must be >= 1", n)
	}
	return n, nil
}

// parseRefreshInterval parses REFRESH_INTERVAL; unparseable or non-positive fails startup rather than silently falling back.
func parseRefreshInterval(s string) (time.Duration, error) {
	if s == "" {
		return defaultRefreshInterval, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid REFRESH_INTERVAL %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid REFRESH_INTERVAL %q: must be positive", s)
	}
	return d, nil
}

// parseReplayInterval parses WEBHOOK_REPLAY_INTERVAL; an explicit 0 disables the replayer, accepting missed deliveries stay missed.
func parseReplayInterval(s string) (time.Duration, error) {
	if s == "" {
		return defaultReplayInterval, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid WEBHOOK_REPLAY_INTERVAL %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid WEBHOOK_REPLAY_INTERVAL %q: must not be negative", s)
	}
	return d, nil
}

// parseWebhookReorderWindow parses WEBHOOK_REORDER_WINDOW; the ceiling bounds risking GitHub's own delivery timeout.
func parseWebhookReorderWindow(s string) (time.Duration, error) {
	if s == "" {
		return defaultWebhookReorderWindow, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid WEBHOOK_REORDER_WINDOW %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid WEBHOOK_REORDER_WINDOW %q: must not be negative (0 dispatches on arrival)", s)
	}
	if d > maxWebhookReorderWindow {
		return 0, fmt.Errorf("invalid WEBHOOK_REORDER_WINDOW %q: must be <= %s (every delivery waits it, and GitHub times deliveries out)", s, maxWebhookReorderWindow)
	}
	return d, nil
}

// parsePassthroughDebounce parses PASSTHROUGH_DEBOUNCE; a fat-fingered value must fail loudly at boot, not wedge the API.
func parsePassthroughDebounce(s string) (time.Duration, error) {
	if s == "" {
		return defaultPassthroughDebounce, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid PASSTHROUGH_DEBOUNCE %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid PASSTHROUGH_DEBOUNCE %q: must not be negative (0 disables)", s)
	}
	if d > maxPassthroughDebounce {
		return 0, fmt.Errorf("invalid PASSTHROUGH_DEBOUNCE %q: must be <= %s", s, maxPassthroughDebounce)
	}
	return d, nil
}

// GitHubAppConfigured checks only the ID; AppPrivateKeyPEM validates the key separately.
func (c Config) GitHubAppConfigured() bool {
	return c.GitHubAppID != ""
}

// AppPrivateKeyPEM prefers the path over the inline value; a wholly unset key returns (nil, nil), never an error.
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

// unescapeNewlines turns a literal "\n"-escaped single-line PEM into a real multi-line one; real newlines pass through.
func unescapeNewlines(s string) string {
	if strings.Contains(s, "\n") {
		return s
	}
	return strings.ReplaceAll(s, `\n`, "\n")
}

// parseOrigins defaults an empty ALLOWED_ORIGINS to ["*"], safe because the mirror isolates data by token fingerprint.
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
func parseAdmins(s string) set.Set[string] {
	out := set.New[string]()
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out.Add(strings.ToLower(p))
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
		// Falls back to a fixed, insecure key rather than crash; set SESSION_SECRET in production.
		slog.Error("could not generate session secret; set SESSION_SECRET", "error", err)
		return []byte("insecure-fallback-session-key")
	}
	slog.Warn("SESSION_SECRET not set; using a random per-process key (sessions reset on restart)")
	return b
}
