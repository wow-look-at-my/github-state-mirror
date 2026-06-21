package ghclient

import "context"

// RateLimitResource is one GitHub rate-limit bucket (core, graphql, search, ...).
type RateLimitResource struct {
	Limit     int   `json:"limit"`
	Remaining int   `json:"remaining"`
	Used      int   `json:"used"`
	Reset     int64 `json:"reset"` // Unix epoch seconds when the window resets
}

// RateLimitResponse is the GET /rate_limit payload.
type RateLimitResponse struct {
	Resources map[string]RateLimitResource `json:"resources"`
}

// GetRateLimit fetches GET /rate_limit using the token in ctx. It does not count
// against the limit it reports. The returned map is keyed by resource name
// ("core", "graphql", "search", ...).
func (c *Client) GetRateLimit(ctx context.Context) (RateLimitResponse, error) {
	var resp RateLimitResponse
	if err := c.doJSON(ctx, "GET", "/rate_limit", nil, &resp); err != nil {
		return RateLimitResponse{}, err
	}
	return resp, nil
}
