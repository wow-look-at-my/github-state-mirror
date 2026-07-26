package ghclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// Fingerprint returns a stable, non-reversible identifier for a token (the hex
// SHA-256 of the raw token; the raw token is never stored or logged). It is the
// cache partition key for tokens that are definitively NOT a user credential
// (e.g. GitHub App installation tokens, which 403 on GET /user): those keep
// per-token isolation. User tokens are partitioned per USER instead — see
// ResolveTokenIdentity and requireAuth in internal/api/router.go.
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
	// retryBackoff overrides the transient-retry backoff schedule (nil = the
	// defaults). See SetRetryBackoff.
	retryBackoff []time.Duration
}

// RateObserver receives every GitHub API response this client sees, so the
// server can passively record the X-RateLimit-* headers GitHub attaches (see
// internal/ratemeter). identity is the principal from the request context
// when one is set, else a label derived from the credential's shape — never
// the raw token value. name is the principal's verified display name from
// the same context ("" when none).
type RateObserver func(identity, name string, resp *http.Response)

// SetRateObserver installs the rate observer. Call it once during startup
// wiring, before the client serves requests: the field is read without
// synchronization on the hot path.
func (c *Client) SetRateObserver(obs RateObserver) { c.rateObserver = obs }

// ExchangeObserver receives every HTTP exchange this client performs against
// GitHub — identity resolution (/user), app verification (/app), token mints,
// GraphQL syncs, the consistency checker's fetches, rate-limit polls — with
// its REAL measured duration (request sent → response headers received).
// Each retry attempt is a separate real request and is observed separately.
// identity/name follow the RateObserver convention: the ctx principal when
// set, else a credential-shape label ("app-jwt", "token:<fp12>",
// "anonymous") — never a raw token. status is 0 when the exchange failed
// before a response arrived.
type ExchangeObserver func(identity, name, method, path string, status int, start time.Time, duration time.Duration)

// SetExchangeObserver installs the exchange observer by wrapping the client's
// transport, so EVERY request the client makes — through any helper, present
// or future — is timed at one choke point. Call it once during startup
// wiring, before the client serves requests.
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

// exchangeIdentity labels a call for the observers: the ctx principal (with
// its verified name) when one is set, else a label derived from the
// credential's shape — never the raw token value.
func exchangeIdentity(ctx context.Context, credential string) (identity, name string) {
	identity = actor.FromContext(ctx)
	if identity != "" {
		// Only pair a name with a ctx-resolved principal: names are set
		// alongside the actor, so a credential-shape fallback identity must
		// never borrow one.
		return identity, actor.NameFromContext(ctx)
	}
	switch {
	case credential == "":
		return "anonymous", ""
	case strings.Count(credential, ".") == 2:
		// A JWT (dot-separated structure — GitHub tokens never contain dots)
		// is the app's own credential.
		return "app-jwt", ""
	default:
		return "token:" + Fingerprint(credential)[:12], ""
	}
}

// observeRate reports a response to the rate observer (if any). The identity
// is the principal in ctx when set (requireAuth / the background app
// sessions); otherwise it is derived from the credential that made the call:
// a JWT (dot-separated structure — GitHub tokens never contain dots) is the
// app's own credential ("app-jwt"), anything else becomes a short,
// non-reversible token fingerprint.
func (c *Client) observeRate(ctx context.Context, credential string, resp *http.Response) {
	if c.rateObserver == nil || resp == nil {
		return
	}
	identity, name := exchangeIdentity(ctx, credential)
	c.rateObserver(identity, name, resp)
}

// New creates a Client targeting the public GitHub API. The client carries no
// token of its own: every request authenticates with the token in its context
// (see WithToken), set per-request from the caller's Authorization header or,
// for background refreshes, from a GitHub App installation token.
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

// BaseURL returns the GitHub API base URL this client targets (normally
// "https://api.github.com"). The HTTP passthrough proxy uses it so that
// forwarded requests reach the same upstream the cache fetchers do, including a
// fake server in tests.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// ErrBadCredential marks a token GitHub itself rejected (401 on GET /user):
// the credential is invalid or revoked. Callers translate it into their own
// 401 — distinct from a transient resolution failure, which must NOT be
// treated as an invalid credential.
var ErrBadCredential = errors.New("github rejected the credential")

// TokenIdentity is the resolved identity of a bearer token, learned from
// GET /user with that token.
type TokenIdentity struct {
	// IsUser reports whether the token authenticates a GitHub user account.
	// False is a DEFINITIVE verdict (GitHub answered /user with a non-rate-limit
	// 403 or a 404 — e.g. an installation token, which has no user identity),
	// not a failure.
	IsUser bool
	// ID is the user's numeric id — stable across login renames, and GitHub
	// never recycles ids. Zero when !IsUser.
	ID int64
	// Login is the user's current login. Empty when !IsUser.
	Login string
}

// ResolveTokenIdentity resolves the token in ctx to its GitHub user via
// GET /user, caching the answer — including the definitive "not a user"
// verdict — per token, so GitHub is asked once per unique token.
//
// Outcomes:
//   - user token: TokenIdentity{IsUser: true, ID, Login}, cached
//   - definitively not a user (403/404 — installation tokens and the like):
//     TokenIdentity{IsUser: false}, cached
//   - invalid credential (401): an error wrapping ErrBadCredential, uncached
//   - anything transient (network error, 5xx, 429, a rate-limited 403): an
//     error, and NOTHING is cached — the next call retries
//
// A 403 counts as transient (not a verdict) when it looks like rate limiting
// (Retry-After, or X-RateLimit-Remaining: 0): caching "not a user" for a
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
			// A 200 missing id/login is malformed; failing (transient, uncached)
			// beats partitioning on garbage.
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
		// Definitive: a valid credential with no user identity behind it (e.g.
		// a GitHub App installation token). Cache the verdict so we never
		// re-ask for this token.
		ident := TokenIdentity{IsUser: false}
		c.identityCache.Store(token, ident)
		return ident, nil

	default:
		// 5xx, 429, rate-limited 403, anything unexpected: transient. Cache
		// nothing so the next request retries.
		data, _ := io.ReadAll(resp.Body)
		return TokenIdentity{}, fmt.Errorf("resolve token identity: GET /user: %d %s", resp.StatusCode, string(data))
	}
}

// looksRateLimited reports whether a 4xx response is GitHub rate limiting
// rather than a permissions answer (primary limit: X-RateLimit-Remaining: 0;
// secondary/abuse limits: Retry-After).
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
// App JWT is signed with the app's private key (RS256); GitHub only returns 200
// if that signature checks out against the public key it holds for the app, so a
// successful response is unforgeable proof that the caller holds the app's
// private key — exactly the "GitHub agrees you are app X" assertion. The result
// is cached per JWT (a caller reuses one JWT for its ~9-minute validity), so this
// costs one upstream call per JWT, not per request.
//
// The returned identity is meant to be used as a stable cache partition for a
// trusted first-party app caller (e.g. a webhook handler) whose underlying
// installation tokens rotate hourly: every one of those tokens proves the same
// app identity, so they all share one bucket.
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

// Transient-retry tuning for doJSON. GitHub (and the CDN in front of it)
// intermittently answers 502/503/504 on otherwise-fine requests -- the very
// reason the GraphQL fetches page at 5 repos each -- and a single blip used to
// fail a whole multi-page owner fetch. Authoritative statuses (401/403/404/...)
// are never retried: the reveal layer and deny-cache semantics depend on them.
// GraphQL-level errors[] bodies arrive as HTTP 200 and stay fail-fast too (the
// callers inspect them after doJSON returns).
const doJSONAttempts = 3

// retryAfterCap bounds how long a parseable Retry-After header can stretch one
// backoff, so a huge value can't wedge a deadline-bounded fetch.
const retryAfterCap = 10 * time.Second

// defaultRetryBackoff is the sleep before attempts 2, 3, ... (the last entry
// repeats).
var defaultRetryBackoff = []time.Duration{500 * time.Millisecond, 2 * time.Second}

// SetRetryBackoff overrides doJSON's transient-retry backoff schedule (tests
// use zero delays so retries don't really sleep). The i-th entry is the sleep
// before attempt i+2; the last entry repeats. Call it during wiring, like
// SetRateObserver: the field is read without synchronization.
func (c *Client) SetRetryBackoff(delays []time.Duration) { c.retryBackoff = delays }

// retryableStatus reports whether an HTTP status is a transient upstream
// failure worth retrying (never an authoritative 4xx answer).
func retryableStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusTooManyRequests:
		return true
	}
	return false
}

// retryDelay returns the sleep before the given attempt (attempt >= 2): the
// configured backoff, raised to a retryable response's parseable Retry-After
// (capped at retryAfterCap).
func (c *Client) retryDelay(attempt int, resp *http.Response) time.Duration {
	backoff := c.retryBackoff
	if len(backoff) == 0 {
		backoff = defaultRetryBackoff
	}
	i := attempt - 2
	if i >= len(backoff) {
		i = len(backoff) - 1
	}
	d := backoff[i]
	if ra := retryAfterDelay(resp); ra > d {
		d = ra
	}
	return d
}

// retryAfterDelay parses a response's Retry-After header (seconds form; zero
// when absent/unparseable), capped at retryAfterCap.
func retryAfterDelay(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	secs, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || secs <= 0 {
		return 0
	}
	if d := time.Duration(secs) * time.Second; d < retryAfterCap {
		return d
	}
	return retryAfterCap
}

// sleepBeforeRetry sleeps the backoff before the given attempt, cut short by
// ctx cancellation (false = don't retry).
func (c *Client) sleepBeforeRetry(ctx context.Context, attempt int, resp *http.Response) bool {
	d := c.retryDelay(attempt, resp)
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Client) doJSON(ctx context.Context, method, path string, body io.Reader, out interface{}) error {
	// Buffer the body once so every retry attempt can resend it from the
	// start (an io.Reader is consumed by the first attempt).
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return err
		}
	}
	url := c.baseURL + path
	// Authenticate with the token carried in the context (caller's bearer token
	// or a GitHub App installation token). Requests without one are sent
	// unauthenticated and will be rejected by GitHub.
	token := tokenFromContext(ctx)

	var resp *http.Response
	for attempt := 1; ; attempt++ {
		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err = c.httpClient.Do(req)
		if err != nil {
			// Network errors are transient EXCEPT the caller's own
			// cancellation/deadline -- retrying those only delays the exit.
			if attempt >= doJSONAttempts || ctx.Err() != nil ||
				errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if !c.sleepBeforeRetry(ctx, attempt+1, nil) {
				return err
			}
			continue
		}
		c.observeRate(ctx, token, resp)

		if attempt < doJSONAttempts && retryableStatus(resp.StatusCode) {
			// Drain a little and close so the connection is reusable, then
			// back off (honoring a capped Retry-After) and resend.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			if !c.sleepBeforeRetry(ctx, attempt+1, resp) {
				return fmt.Errorf("github api %s %s: %d (retry canceled: %w)", method, path, resp.StatusCode, ctx.Err())
			}
			continue
		}
		break
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github api %s %s: %d %s", method, path, resp.StatusCode, string(data))
	}

	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
