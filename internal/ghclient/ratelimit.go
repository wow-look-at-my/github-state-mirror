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
//
// Rate is GitHub's deprecated top-level alias for resources.core. GitHub still
// sends it on every answer, so the mirror's own rebuild of this endpoint
// (internal/api/respcache_ratelimit.go) sends it too: a caller that reads it
// must not silently start reading nothing the day it points at the mirror.
// Omitted when core is not known, never guessed.
type RateLimitResponse struct {
	Resources map[string]RateLimitResource `json:"resources"`
	Rate      *RateLimitResource           `json:"rate,omitempty"`
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
