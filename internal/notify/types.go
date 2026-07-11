// Package notify implements subscriber notifications: authenticated API
// clients register webhook endpoints with the mirror, and after the mirror's
// synchronous dispatcher applies a GitHub delivery to global truth, the mirror
// POSTs a compact, HMAC-signed notification to every matching, authorized
// subscription. Consumers key off these post-ingest notifications instead of
// racing their own GitHub webhooks against the mirror's ingestion.
//
// Subscriptions are service CONFIG, not cache: they live in their own SQLite
// file (see store.go) that the main DB's SchemaVersion nuke never touches.
package notify

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Limits enforced by validation.
const (
	// MaxPerPrincipal caps how many subscriptions one principal may hold.
	MaxPerPrincipal = 20
	// maxFilters caps the repo and event filter lists.
	maxFilters = 50
	// secret length bounds (the HMAC key subscribers verify deliveries with).
	minSecretLen = 16
	maxSecretLen = 256
)

// Subscription is one registered notification endpoint. Secret is the HMAC
// key used to sign every delivery — it is stored to sign with, never returned
// by any API response and never logged.
type Subscription struct {
	ID        string
	Principal string
	URL       string
	Secret    string
	// RepoFilters restricts which repos notify this subscription: each entry
	// is "owner" (every repo of that owner) or "owner/repo", lowercased.
	// Empty = all repos the principal may see.
	RepoFilters []string
	// EventFilters restricts which GitHub event types notify this
	// subscription ("push", "pull_request", ...). Empty = all events.
	EventFilters []string
	Active       bool
	// ConsecutiveFailures counts terminal delivery failures since the last
	// success; reaching the auto-disable threshold flips Active off.
	ConsecutiveFailures int64
	DisabledReason      string
	CreatedAt           string // RFC3339Nano UTC
	UpdatedAt           string // RFC3339Nano UTC
	LastSuccessAt       string // RFC3339Nano UTC, '' = never
	LastFailureAt       string // RFC3339Nano UTC, '' = never
	LastError           string // short description of the last terminal failure
}

// MatchesEvent reports whether the subscription's event filter admits the
// GitHub event type. An empty filter admits everything.
func (s Subscription) MatchesEvent(eventType string) bool {
	if len(s.EventFilters) == 0 {
		return true
	}
	for _, e := range s.EventFilters {
		if e == eventType {
			return true
		}
	}
	return false
}

// MatchesRepo reports whether the subscription's repo filter admits
// owner/repo (case-insensitively; filters are stored lowercased). An empty
// filter admits everything.
func (s Subscription) MatchesRepo(owner, repo string) bool {
	if len(s.RepoFilters) == 0 {
		return true
	}
	o := strings.ToLower(owner)
	or := o + "/" + strings.ToLower(repo)
	for _, f := range s.RepoFilters {
		if f == o || f == or {
			return true
		}
	}
	return false
}

// NewSubscription carries the caller-supplied fields of a create request.
type NewSubscription struct {
	URL    string   `json:"url"`
	Secret string   `json:"secret"`
	Repos  []string `json:"repos"`
	Events []string `json:"events"`
}

// Patch carries a partial update. Nil fields are left unchanged. Setting
// Active=true resets ConsecutiveFailures and clears DisabledReason — the
// re-enable path after an auto-disable.
type Patch struct {
	URL    *string   `json:"url"`
	Secret *string   `json:"secret"`
	Repos  *[]string `json:"repos"`
	Events *[]string `json:"events"`
	Active *bool     `json:"active"`
}

// ValidationError describes a rejected subscription field; the API maps it to
// a 400 response with the message.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func invalidf(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// repoFilterRe admits "owner" or "owner/repo" entries (already lowercased):
// GitHub logins/repo names are alphanumerics plus ., _, -.
var repoFilterRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}(/[a-z0-9._-]{1,100})?$`)

// eventFilterRe admits GitHub event names ("push", "pull_request", ...).
var eventFilterRe = regexp.MustCompile(`^[a-z_]{1,64}$`)

// NormalizeAndValidate checks every caller-controlled field, lowercasing the
// repo filters in place. Fail closed: any dubious value is rejected with a
// message naming the field.
func (s *Subscription) NormalizeAndValidate() error {
	if err := validateEndpointURL(s.URL); err != nil {
		return err
	}
	if n := len(s.Secret); n < minSecretLen || n > maxSecretLen {
		return invalidf("secret must be between %d and %d characters", minSecretLen, maxSecretLen)
	}
	if len(s.RepoFilters) > maxFilters {
		return invalidf("repos: at most %d entries", maxFilters)
	}
	for i, f := range s.RepoFilters {
		f = strings.ToLower(strings.TrimSpace(f))
		if !repoFilterRe.MatchString(f) {
			return invalidf("repos[%d]: must be \"owner\" or \"owner/repo\" (letters, digits, . _ -)", i)
		}
		s.RepoFilters[i] = f
	}
	if len(s.EventFilters) > maxFilters {
		return invalidf("events: at most %d entries", maxFilters)
	}
	for i, e := range s.EventFilters {
		if !eventFilterRe.MatchString(e) {
			return invalidf("events[%d]: must match ^[a-z_]{1,64}$", i)
		}
	}
	return nil
}

// validateEndpointURL enforces the delivery-target rules: absolute, no
// userinfo, https (plain http only for loopback hosts so local receivers and
// tests work), and no private/link-local IP literals — cheap SSRF hygiene.
// DNS names are accepted as-is: resolving them here would still be spoofable
// at delivery time (rebinding), so that defense is out of scope.
func validateEndpointURL(raw string) error {
	if raw == "" {
		return invalidf("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return invalidf("url: %v", err)
	}
	if !u.IsAbs() {
		return invalidf("url must be absolute")
	}
	if u.User != nil {
		return invalidf("url must not contain userinfo")
	}
	host := u.Hostname()
	if host == "" {
		return invalidf("url must have a host")
	}
	switch u.Scheme {
	case "https":
		// Always acceptable (subject to the IP-literal check below).
	case "http":
		if !isLoopbackHost(host) {
			return invalidf("url scheme must be https (plain http is allowed only for loopback hosts)")
		}
	default:
		return invalidf("url scheme must be https (got %q)", u.Scheme)
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
		// 10/8, 172.16/12, 192.168/16, fc00::/7 (IsPrivate) and 169.254/16,
		// fe80::/10 (IsLinkLocalUnicast): the mirror must not be pointed at
		// internal infrastructure via an IP literal. 0.0.0.0/:: are not
		// dialable targets either.
		if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return invalidf("url host must not be a private, link-local, or unspecified IP literal")
		}
	}
	return nil
}

// isLoopbackHost reports whether host names the local machine: "localhost" or
// a loopback IP literal (127.0.0.0/8, ::1).
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DeriveDBPath derives the default subscriptions DB path from the main cache
// DB path: strip a trailing ".db" and append "-subscriptions.db"
// (github-mirror.db -> github-mirror-subscriptions.db).
func DeriveDBPath(dbPath string) string {
	return strings.TrimSuffix(dbPath, ".db") + "-subscriptions.db"
}
