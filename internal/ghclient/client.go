package ghclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
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

// Fingerprint returns a stable, non-reversible identifier for a token, suitable
// for use as a per-credential cache partition key. Cached data is keyed by this
// fingerprint (not the GitHub login) so that two distinct tokens never share a
// cache bucket — a narrow-scoped token can never read data a broader token
// fetched, even when both belong to the same GitHub user. The raw token is
// never stored or logged.
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type Client struct {
	httpClient       *http.Client
	baseURL          string
	defaultToken     string   // optional fallback for background refreshes
	actorCache       sync.Map // token -> GitHub login
	appIdentityCache sync.Map // app JWT -> AppIdentity
}

// New creates a Client with an optional default token used when no token is in the context.
func New(defaultToken string) *Client {
	return &Client{
		httpClient:   &http.Client{},
		baseURL:      defaultBaseURL,
		defaultToken: defaultToken,
	}
}

// NewWithBaseURL creates a Client pointing at a custom base URL (for testing).
func NewWithBaseURL(defaultToken, baseURL string) *Client {
	return &Client{
		httpClient:   &http.Client{},
		baseURL:      baseURL,
		defaultToken: defaultToken,
	}
}

// ResolveActor resolves the GitHub login for the token in the given context.
// Results are cached in memory so /user is only called once per unique token.
func (c *Client) ResolveActor(ctx context.Context) (string, error) {
	token := tokenFromContext(ctx)
	if token == "" {
		token = c.defaultToken
	}
	if token == "" {
		return "", nil
	}

	if login, ok := c.actorCache.Load(token); ok {
		return login.(string), nil
	}

	var resp userResp
	if err := c.doJSON(ctx, "GET", "/user", nil, &resp); err != nil {
		return "", err
	}

	c.actorCache.Store(token, resp.Login)
	return resp.Login, nil
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

// Forward proxies an arbitrary request to the upstream GitHub API and returns
// the raw response for the caller to copy verbatim. It is the passthrough path
// for endpoints the mirror does not cache: the request method, path, query, and
// body are forwarded unchanged, authenticated with the caller's own token (from
// context) so GitHub applies the caller's authorization. The caller is
// responsible for closing the returned response body.
func (c *Client) Forward(ctx context.Context, method, path, rawQuery string, in http.Header, body io.Reader) (*http.Response, error) {
	u := c.baseURL + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	// Forward the caller's content negotiation and API-version headers so media
	// types like application/vnd.github.diff are honored upstream.
	for _, h := range []string{"Accept", "Content-Type", "X-Github-Api-Version"} {
		if v := in.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	token := tokenFromContext(ctx)
	if token == "" {
		token = c.defaultToken
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.httpClient.Do(req)
}

func (c *Client) doJSON(ctx context.Context, method, path string, body io.Reader, out interface{}) error {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	// Prefer token from context (passthrough from caller), fall back to default.
	token := tokenFromContext(ctx)
	if token == "" {
		token = c.defaultToken
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
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
