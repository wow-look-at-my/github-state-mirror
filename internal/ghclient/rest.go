package ghclient

import (
	"context"
	"encoding/json"
	"fmt"
)

// maxOpenPullPages bounds the open-PR list walk (100 PRs per page). A repo
// with more than this many open PRs fails the fetch rather than absorb a
// silently truncated list.
const maxOpenPullPages = 20

// ListOpenPulls fetches every open PR of a repo via the REST list endpoint
// (state=open, per_page=100, paginated), returning the raw PR objects for the
// caller to parse. Used by the repo-pulls fetcher; the response objects are
// REST-complete (node_id, body, auto_merge, ...) unlike the GraphQL org fetch.
func (c *Client) ListOpenPulls(ctx context.Context, owner, repo string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	for page := 1; ; page++ {
		if page > maxOpenPullPages {
			return nil, fmt.Errorf("list open pulls %s/%s: more than %d pages of open PRs", owner, repo, maxOpenPullPages)
		}
		path := fmt.Sprintf("/repos/%s/%s/pulls?state=open&per_page=100&page=%d", owner, repo, page)
		var batch []json.RawMessage
		if err := c.doJSON(ctx, "GET", path, nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}
