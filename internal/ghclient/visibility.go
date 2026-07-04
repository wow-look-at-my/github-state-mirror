package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// ownerRepoVisibilityQuery is orgRepoVisibilityQuery's owner-agnostic twin
// (repositoryOwner resolves Users too), still checker-private. Two deliberate
// differences: it selects the visibility ENUM (so "internal" is not conflated
// with private) plus isArchived, and it does NOT filter isArchived: false --
// so archived repos are classifiable (a cached repo missing from the org data
// can be positively identified as archived rather than deleted/renamed).
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
