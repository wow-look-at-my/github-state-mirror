package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// ownerPRFields is the owner-agnostic PR selection, separate from prFields
// (the identity-locked /graphql route's frozen contract).
// see docs/ghclient.md
const ownerPRFields = `
  number
  title
  url
  isDraft
  createdAt
  updatedAt
  additions
  deletions
  mergeable
  author { login avatarUrl url }
  headRefName
  baseRefName
  headRefOid
  labels(first: 100) { nodes { name color } }
  reviewRequests { totalCount }
  autoMergeRequest { mergeMethod }
  commits(last: 1) {
    nodes {
      commit {
        statusCheckRollup { state }
      }
    }
  }
`

// ownerDataQuery resolves both Organization and User accounts via
// repositoryOwner(login:), unlike the identity-locked orgDataQuery.
// see docs/ghclient.md
const ownerDataQuery = `
query($owner: String!, $repoCursor: String) {
  repositoryOwner(login: $owner) {
    repositories(first: 5, after: $repoCursor, isArchived: false, ownerAffiliations: OWNER, orderBy: {field: PUSHED_AT, direction: DESC}) {
      totalCount
      pageInfo { hasNextPage endCursor }
      nodes {
        name
        nameWithOwner
        url
        isDisabled
        isArchived
        visibility
        pushedAt
        owner { login avatarUrl url }
        defaultBranchRef {
          name
          target {
            ... on Commit {
              statusCheckRollup { state }
            }
          }
        }
        pullRequests(first: 100, states: OPEN, orderBy: {field: UPDATED_AT, direction: DESC}) {
          pageInfo { hasNextPage endCursor }
          nodes {` + ownerPRFields + `}
        }
      }
    }
  }
}
`

// ownerRepoPRsQuery paginates a single repository's open PRs past the first
// 100 for GetOwnerData -- same shape as repoPRsQuery (repository(owner:,name:)
// already resolves user-owned repos) but with the ownerPRFields selection so
// follow-up pages carry the same fields as page one.
const ownerRepoPRsQuery = `
query($owner: String!, $name: String!, $prCursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: 100, after: $prCursor, states: OPEN, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {` + ownerPRFields + `}
    }
  }
}
`

// gqlOwnerResponse unmarshals ownerDataQuery. repositoryOwner is a nullable
// field: an unknown login yields data.repositoryOwner == null with NO GraphQL
// error, so GetOwnerData checks the pointer explicitly.
type gqlOwnerResponse struct {
	Data struct {
		RepositoryOwner *struct {
			Repositories struct {
				TotalCount int         `json:"totalCount"`
				PageInfo   gqlPageInfo `json:"pageInfo"`
				Nodes      []gqlRepo   `json:"nodes"`
			} `json:"repositories"`
		} `json:"repositoryOwner"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// OwnerPageFunc reports cumulative repos fetched and the connection's total (0 if unreported).
type OwnerPageFunc func(reposFetched, reposTotal int)

// GetOwnerData fetches all non-archived repos and open PRs for any owner (org or user).
func (c *Client) GetOwnerData(ctx context.Context, ownerLogin string) (*OrgData, error) {
	return c.GetOwnerDataWithProgress(ctx, ownerLogin, nil)
}

// GetOwnerDataWithProgress is GetOwnerData with an optional per-page progress
// hook (nil = no reporting, identical to GetOwnerData). The consistency
// checker uses it to stream "N of M repos fetched" while a large owner pages
// through at 5 repos per page.
func (c *Client) GetOwnerDataWithProgress(ctx context.Context, ownerLogin string, onPage OwnerPageFunc) (*OrgData, error) {
	result := &OrgData{
		PRsByRepo:  make(map[string][]dbgen.PullRequest),
		LabelsByPR: make(map[string]map[int64][]dbgen.PrLabel),
	}

	var repoCursor *string
	for {
		vars := map[string]interface{}{
			"owner": ownerLogin,
		}
		if repoCursor != nil {
			vars["repoCursor"] = *repoCursor
		}

		body, err := json.Marshal(map[string]interface{}{
			"query":     ownerDataQuery,
			"variables": vars,
		})
		if err != nil {
			return nil, err
		}

		var resp gqlOwnerResponse
		if err := c.doJSON(ctx, "POST", "/graphql", bytes.NewReader(body), &resp); err != nil {
			return nil, err
		}
		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
		}
		if resp.Data.RepositoryOwner == nil {
			// A null repositoryOwner means an unknown login (GraphQL emits no error here); fail loudly.
			return nil, fmt.Errorf("repositoryOwner(login: %q) resolved to null (unknown owner?)", ownerLogin)
		}

		repos := resp.Data.RepositoryOwner.Repositories
		for _, gr := range repos.Nodes {
			if dropForeignRepoNode(ownerLogin, gr.NameWithOwner, gr.Owner.Login) {
				continue
			}
			repo := convertRepo(ownerLogin, gr)
			result.Repos = append(result.Repos, repo)

			repoKey := repo.NameWithOwner
			for _, gpr := range gr.PullRequests.Nodes {
				addPR(result, ownerLogin, gr.Name, repoKey, gpr)
			}

			// Repos with more than 100 open PRs need follow-up pages.
			if gr.PullRequests.PageInfo.HasNextPage {
				if err := c.fetchRemainingOwnerPRs(ctx, result, ownerLogin, gr.Name, repoKey, gr.PullRequests.PageInfo.EndCursor); err != nil {
					return nil, err
				}
			}
		}

		if onPage != nil {
			onPage(len(result.Repos), repos.TotalCount)
		}

		if !repos.PageInfo.HasNextPage {
			break
		}
		repoCursor = &repos.PageInfo.EndCursor
	}

	return result, nil
}

// fetchRemainingOwnerPRs pages through a single repo's remaining open PRs with
// the owner-agnostic PR selection, appending them into result.
func (c *Client) fetchRemainingOwnerPRs(ctx context.Context, result *OrgData, ownerLogin, repoName, repoKey, startCursor string) error {
	cursor := startCursor
	for {
		body, err := json.Marshal(map[string]interface{}{
			"query": ownerRepoPRsQuery,
			"variables": map[string]interface{}{
				"owner":    ownerLogin,
				"name":     repoName,
				"prCursor": cursor,
			},
		})
		if err != nil {
			return err
		}

		var resp repoPRsResponse
		if err := c.doJSON(ctx, "POST", "/graphql", bytes.NewReader(body), &resp); err != nil {
			return err
		}
		if len(resp.Errors) > 0 {
			return fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
		}

		prs := resp.Data.Repository.PullRequests
		for _, gpr := range prs.Nodes {
			addPR(result, ownerLogin, repoName, repoKey, gpr)
		}

		if !prs.PageInfo.HasNextPage {
			return nil
		}
		cursor = prs.PageInfo.EndCursor
	}
}
