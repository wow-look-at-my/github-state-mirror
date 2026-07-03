package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// orgRepoVisibilityQuery is a consistency-checker-private query: just each
// non-archived repo's name and visibility. It is deliberately SEPARATE from
// orgDataQuery — that selection set is the cached route's contract, locked
// byte-identical to GitHub by the identity test, so the checker must never
// extend it. The response here is tiny, so it pages big (100 per page).
const orgRepoVisibilityQuery = `
query($org: String!, $repoCursor: String) {
  organization(login: $org) {
    repositories(first: 100, after: $repoCursor, isArchived: false) {
      pageInfo { hasNextPage endCursor }
      nodes { name isPrivate }
    }
  }
}
`

// gqlVisibilityResponse unmarshals orgRepoVisibilityQuery.
type gqlVisibilityResponse struct {
	Data struct {
		Organization struct {
			Repositories struct {
				PageInfo gqlPageInfo `json:"pageInfo"`
				Nodes    []struct {
					Name      string `json:"name"`
					IsPrivate bool   `json:"isPrivate"`
				} `json:"nodes"`
			} `json:"repositories"`
		} `json:"organization"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// OrgRepoVisibility returns whether each of orgLogin's non-archived repos is
// private, keyed by repo name. The consistency check uses it to distinguish
// "not cached because the scope's token cannot see the repo" from a genuine
// cache miss.
func (c *Client) OrgRepoVisibility(ctx context.Context, orgLogin string) (map[string]bool, error) {
	out := make(map[string]bool)

	var repoCursor *string
	for {
		vars := map[string]interface{}{"org": orgLogin}
		if repoCursor != nil {
			vars["repoCursor"] = *repoCursor
		}
		body, err := json.Marshal(map[string]interface{}{
			"query":     orgRepoVisibilityQuery,
			"variables": vars,
		})
		if err != nil {
			return nil, err
		}

		var resp gqlVisibilityResponse
		if err := c.doJSON(ctx, "POST", "/graphql", bytes.NewReader(body), &resp); err != nil {
			return nil, err
		}
		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
		}

		repos := resp.Data.Organization.Repositories
		for _, n := range repos.Nodes {
			out[n.Name] = n.IsPrivate
		}
		if !repos.PageInfo.HasNextPage {
			break
		}
		repoCursor = &repos.PageInfo.EndCursor
	}

	return out, nil
}
