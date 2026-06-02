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
	httpClient   *http.Client
	baseURL      string
	defaultToken string   // optional fallback for background refreshes
	actorCache   sync.Map // token -> GitHub login
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
