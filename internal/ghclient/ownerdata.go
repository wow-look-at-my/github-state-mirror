package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// ownerPRFields is the PR selection set for the OWNER-AGNOSTIC queries below
// (the App-driven fleet sync and the consistency checker). It is deliberately
// SEPARATE from prFields: that selection is the identity-locked /graphql
// route's contract and must never be extended. This one may grow, and already
// does: labels page at 100 (prFields' 10 silently truncates busy PRs) and
// autoMergeRequest is selected so drift in the armed auto-merge state is
// visible to the checker.
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

// ownerDataQuery is orgDataQuery's owner-agnostic twin: repositoryOwner(login:)
// resolves BOTH Organization and User accounts (organization(login:) errors on
// a User), so the App-driven paths -- the periodic fleet refresher and the
// consistency checker, whose installations can be user accounts -- use it.
// Selection matches orgDataQuery (same small first: 5 paging, for the same
// 502-avoidance reason) plus isArchived per repo and the ownerPRFields extras.
// orgDataQuery itself stays byte-untouched: it is the identity-locked cached
// route's contract.
const ownerDataQuery = `
query($owner: String!, $repoCursor: String) {
  repositoryOwner(login: $owner) {
    repositories(first: 5, after: $repoCursor, isArchived: false, orderBy: {field: PUSHED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        name
        nameWithOwner
        url
        isDisabled
        isArchived
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
				PageInfo gqlPageInfo `json:"pageInfo"`
				Nodes    []gqlRepo   `json:"nodes"`
			} `json:"repositories"`
		} `json:"repositoryOwner"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// GetOwnerData fetches all non-archived repos and open PRs for any repository
// owner -- Organization or User -- via the owner-agnostic GraphQL query. Same
// pagination and conversion as GetOrgData; used by the App-driven paths (the
// periodic fleet refresher and the consistency checker), never by the
// identity-locked lazy /graphql route.
func (c *Client) GetOwnerData(ctx context.Context, ownerLogin string) (*OrgData, error) {
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
			// A null repositoryOwner is a silent "no such login" (GraphQL emits
			// no error for a nullable field). Failing loudly matters: treating
			// it as an empty owner would make a checker diff read every cached
			// repo as only_in_cache.
			return nil, fmt.Errorf("repositoryOwner(login: %q) resolved to null (unknown owner?)", ownerLogin)
		}

		repos := resp.Data.RepositoryOwner.Repositories
		for _, gr := range repos.Nodes {
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
