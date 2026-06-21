package ghclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// RESTResponse is a GitHub REST response body plus the small amount of metadata
// the mirror can safely replay after normalizing the JSON body.
type RESTResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// GetREST fetches a REST path from GitHub using the caller token in ctx.
func (c *Client) GetREST(ctx context.Context, path string) (RESTResponse, error) {
	return c.GetRESTWithHeaders(ctx, path, nil)
}

// GetRESTWithHeaders fetches a REST path from GitHub using selected caller
// headers that affect the response representation.
func (c *Client) GetRESTWithHeaders(ctx context.Context, path string, headers http.Header) (RESTResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return RESTResponse{}, err
	}
	if accept := headers.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	} else {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	if version := headers.Get("X-GitHub-Api-Version"); version != "" {
		req.Header.Set("X-GitHub-Api-Version", version)
	}
	if token := tokenFromContext(ctx); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return RESTResponse{}, err
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return RESTResponse{}, readErr
	}
	out := RESTResponse{
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
	}
	if resp.StatusCode >= 500 {
		return out, fmt.Errorf("github api GET %s: %d %s", path, resp.StatusCode, string(body))
	}
	return out, nil
}
