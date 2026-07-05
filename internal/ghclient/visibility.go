package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ownerRepoVisibilityQuery is a consistency-checker-private query: each repo's
// visibility ENUM (so "internal" is not conflated with private) and archive
// state, resolved via repositoryOwner so User accounts work like
// Organizations. It is deliberately SEPARATE from the shared data queries --
// orgDataQuery's selection set is the cached route's contract, locked
// byte-identical to GitHub by the identity test, so the checker must never
// extend it. Deliberately NO isArchived filter: archived repos must be
// classifiable (a cached repo missing from the org data can then be
// positively identified as archived rather than deleted/renamed). The
// response is tiny, so it pages big (100 per page).
const ownerRepoVisibilityQuery = `
query($owner: String!, $repoCursor: String) {
  repositoryOwner(login: $owner) {
    repositories(first: 100, after: $repoCursor) {
      pageInfo { hasNextPage endCursor }
      nodes { name visibility isArchived }
    }
  }
}
`

// gqlOwnerVisibilityResponse unmarshals ownerRepoVisibilityQuery.
type gqlOwnerVisibilityResponse struct {
	Data struct {
		RepositoryOwner *struct {
			Repositories struct {
				PageInfo gqlPageInfo `json:"pageInfo"`
				Nodes    []struct {
					Name       string `json:"name"`
					Visibility string `json:"visibility"`
					IsArchived bool   `json:"isArchived"`
				} `json:"nodes"`
			} `json:"repositories"`
		} `json:"repositoryOwner"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// OwnerRepoVisibility is one repo's live visibility + archive state as GitHub
// answers the App, from the checker-private owner query.
type OwnerRepoVisibility struct {
	Visibility string // "public" | "private" | "internal" (lowercased enum)
	Archived   bool
}

// OwnerRepoVisibilities returns every repo of any owner (Organization or User)
// keyed by name, INCLUDING archived repos, with its visibility and archive
// state. The consistency check uses it to classify missing repos
// (private-and-never-absorbed vs archived vs genuinely gone) and to diff the
// cached visibility column -- the reveal layer's security-load-bearing field.
func (c *Client) OwnerRepoVisibilities(ctx context.Context, ownerLogin string) (map[string]OwnerRepoVisibility, error) {
	out := make(map[string]OwnerRepoVisibility)

	var repoCursor *string
	for {
		vars := map[string]interface{}{"owner": ownerLogin}
		if repoCursor != nil {
			vars["repoCursor"] = *repoCursor
		}
		body, err := json.Marshal(map[string]interface{}{
			"query":     ownerRepoVisibilityQuery,
			"variables": vars,
		})
		if err != nil {
			return nil, err
		}

		var resp gqlOwnerVisibilityResponse
		if err := c.doJSON(ctx, "POST", "/graphql", bytes.NewReader(body), &resp); err != nil {
			return nil, err
		}
		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
		}
		if resp.Data.RepositoryOwner == nil {
			return nil, fmt.Errorf("repositoryOwner(login: %q) resolved to null (unknown owner?)", ownerLogin)
		}

		repos := resp.Data.RepositoryOwner.Repositories
		for _, n := range repos.Nodes {
			out[n.Name] = OwnerRepoVisibility{
				Visibility: strings.ToLower(n.Visibility),
				Archived:   n.IsArchived,
			}
		}
		if !repos.PageInfo.HasNextPage {
			break
		}
		repoCursor = &repos.PageInfo.EndCursor
	}

	return out, nil
}
