package ghclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
)

const defaultBaseURL = "https://api.github.com"

// contextKey is an unexported type for context keys in this package.
type contextKey struct{}

// tokenKey is the context key for the GitHub auth token.
var tokenKey = contextKey{}

// WithToken returns a child context carrying the given GitHub auth token.
func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenKey, token)
}

// tokenFromContext returns the token from context, or empty string if absent.
func tokenFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tokenKey).(string); ok {
		return v
	}
	return ""
}

// Fingerprint is a stable, non-reversible identifier for a token (hex SHA-); the raw token is never stored or logged.
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type Client struct {
	httpClient       *http.Client
	baseURL          string
	identityCache    sync.Map // token -> TokenIdentity (incl. the definitive not-a-user verdict)
	appIdentityCache sync.Map // app JWT -> AppIdentity
	rateObserver     RateObserver
	// retryBackoff overrides the transient-retry backoff; nil uses the defaults.
	retryBackoff []time.Duration
}

// RateObserver receives every GitHub API response this client sees; see docs/ghclient.md's Observers section.
type RateObserver func(identity, name string, resp *http.Response)

// SetRateObserver must be called during startup wiring: the field is read unsynchronized.
func (c *Client) SetRateObserver(obs RateObserver) { c.rateObserver = obs }

// ExchangeObserver receives every real HTTP exchange this client performs, with its measured duration.
type ExchangeObserver func(identity, name, method, path string, status int, start time.Time, duration time.Duration)

// SetExchangeObserver wraps the transport so every request is timed at choke point.
func (c *Client) SetExchangeObserver(obs ExchangeObserver) {
	if obs == nil {
		return
	}
	base := c.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.httpClient.Transport = &timingTransport{base: base, observe: obs}
}

// timingTransport times each RoundTrip and reports it to the exchange
// observer. It never alters the request or response.
type timingTransport struct {
	base    http.RoundTripper
	observe ExchangeObserver
}

func (t *timingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	credential := strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer"))
	identity, name := exchangeIdentity(req.Context(), credential)
	t.observe(identity, name, req.Method, req.URL.Path, status, start, time.Since(start))
	return resp, err
}

// exchangeIdentity labels a call: the ctx principal when set, else a credential-shape label.
func exchangeIdentity(ctx context.Context, credential string) (identity, name string) {
	identity = actor.FromContext(ctx)
	if identity != "" {
		// A credential-shape fallback identity must never borrow this name.
		return identity, actor.NameFromContext(ctx)
	}
	switch {
	case credential == "":
		return "anonymous", ""
	case strings.Count(credential, ".") == 2:
		// GitHub tokens never contain dots; a JWT is the app's own credential.
		return "app-jwt", ""
	default:
		return "token:" + Fingerprint(credential)[:12], ""
	}
}

// observeRate reports a response to the rate observer (if any), labeled per exchangeIdentity.
func (c *Client) observeRate(ctx context.Context, credential string, resp *http.Response) {
	if c.rateObserver == nil || resp == nil {
		return
	}
	identity, name := exchangeIdentity(ctx, credential)
	c.rateObserver(identity, name, resp)
}

// New creates a Client with no token of its own; every request authenticates via WithToken's context value.
func New() *Client {
	return &Client{
		httpClient: &http.Client{},
		baseURL:    defaultBaseURL,
	}
}

// NewWithBaseURL creates a Client pointing at a custom base URL (for testing).
func NewWithBaseURL(baseURL string) *Client {
	return &Client{
		httpClient: &http.Client{},
		baseURL:    baseURL,
	}
}

// BaseURL is the passthrough proxy's upstream target, matching the cache fetchers (a fake server in tests).
func (c *Client) BaseURL() string {
	return c.baseURL
}

// ErrBadCredential marks a on GET /user, distinct from a transient resolution failure.
var ErrBadCredential = errors.New("github rejected the credential")

// TokenIdentity is the resolved identity of a bearer token, learned from
// GET /user with that token.
type TokenIdentity struct {
	// IsUser is a DEFINITIVE verdict (a non-rate-limit / on /user), not a failure.
	IsUser bool
	// ID is the user's numeric id, stable across login renames. when !IsUser.
	ID int64
	// Login is the user's current login. Empty when !IsUser.
	Login string
}

// ResolveTokenIdentity resolves the token in ctx to its GitHub user via
// GET /user, caching the answer — including the definitive "not a user"
// verdict — per token, so GitHub is asked per unique token.
//
// Outcomes:
//   - user token: TokenIdentity{IsUser: true, ID, Login}, cached
//   - definitively not a user (/ — installation tokens and the like):
//     TokenIdentity{IsUser: false}, cached
//   - invalid credential (): an error wrapping ErrBadCredential, uncached
//   - anything transient (network error, 5xx,, a rate-limited): an
//     error, and NOTHING is cached — the next call retries
//
// A counts as transient (not a verdict) when it looks like rate limiting
// (Retry-After, or X-RateLimit-Remaining:): caching "not a user" for a
// rate-limited USER token would silently mis-partition that user for the
// process lifetime.
func (c *Client) ResolveTokenIdentity(ctx context.Context) (TokenIdentity, error) {
	token := tokenFromContext(ctx)
	if token == "" {
		return TokenIdentity{}, errors.New("resolve token identity: no token in context")
	}
	if v, ok := c.identityCache.Load(token); ok {
		return v.(TokenIdentity), nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/user", nil)
	if err != nil {
		return TokenIdentity{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return TokenIdentity{}, fmt.Errorf("resolve token identity: %w", err)
	}
	defer resp.Body.Close()
	c.observeRate(ctx, token, resp)

	switch {
	case resp.StatusCode == http.StatusOK:
		var u struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
			return TokenIdentity{}, fmt.Errorf("resolve token identity: decode /user: %w", err)
		}
		if u.ID == 0 || u.Login == "" {
			// Failing beats partitioning on garbage.
			return TokenIdentity{}, errors.New("resolve token identity: /user response missing id or login")
		}
		ident := TokenIdentity{IsUser: true, ID: u.ID, Login: u.Login}
		c.identityCache.Store(token, ident)
		return ident, nil

	case resp.StatusCode == http.StatusUnauthorized:
		data, _ := io.ReadAll(resp.Body)
		return TokenIdentity{}, fmt.Errorf("%w: 401 %s", ErrBadCredential, string(data))

	case resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusForbidden && !looksRateLimited(resp):
		// Definitive: a valid credential with no user identity behind it.
		ident := TokenIdentity{IsUser: false}
		c.identityCache.Store(token, ident)
		return ident, nil

	default:
		// Transient: cache nothing so the next request retries.
		data, _ := io.ReadAll(resp.Body)
		return TokenIdentity{}, fmt.Errorf("resolve token identity: GET /user: %d %s", resp.StatusCode, string(data))
	}
}

// looksRateLimited reports whether a 4xx response is rate limiting rather than a permissions answer.
func looksRateLimited(resp *http.Response) bool {
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	return resp.Header.Get("X-RateLimit-Remaining") == "0"
}

// AppIdentity is a GitHub App identity proven by a valid App JWT.
type AppIdentity struct {
	ID   int64
	Slug string
}

type appResp struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
}

// VerifyAppIdentity validates a GitHub App JWT by calling GET /app with it. The
// App JWT is signed with the app's private key (RS256); GitHub only returns
// if that signature checks out against the public key it holds for the app, so a
// successful response is unforgeable proof that the caller holds the app's
// private key — exactly the "GitHub agrees you are app X" assertion. The result
// is cached per JWT (a caller reuses JWT for its ~-minute validity), so this
// costs upstream call per JWT, not per request.
//
// The returned identity is meant to be used as a stable cache partition for a
// trusted -party app caller (e.g. a webhook handler) whose underlying
// installation tokens rotate hourly: every of those tokens proves the same
// app identity, so they all share bucket.
func (c *Client) VerifyAppIdentity(ctx context.Context, jwt string) (AppIdentity, error) {
	if v, ok := c.appIdentityCache.Load(jwt); ok {
		return v.(AppIdentity), nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/app", nil)
	if err != nil {
		return AppIdentity{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return AppIdentity{}, err
	}
	defer resp.Body.Close()
	c.observeRate(ctx, jwt, resp)
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return AppIdentity{}, fmt.Errorf("verify app identity: %d %s", resp.StatusCode, string(data))
	}
	var a appResp
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return AppIdentity{}, err
	}
	if a.ID == 0 {
		return AppIdentity{}, fmt.Errorf("verify app identity: response missing app id")
	}
	id := AppIdentity{ID: a.ID, Slug: a.Slug}
	c.appIdentityCache.Store(jwt, id)
	return id, nil
}
