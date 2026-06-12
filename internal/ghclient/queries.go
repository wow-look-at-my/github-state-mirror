package ghclient

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// prFields is the GraphQL selection set for a pull request node, shared between
// the org-level query and the per-repo PR pagination query so the two stay in
// sync and both unmarshal into gqlPR.
const prFields = `
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
  labels(first: 10) { nodes { name color } }
  reviewRequests { totalCount }
  commits(last: 1) {
    nodes {
      commit {
        statusCheckRollup { state }
      }
    }
  }
`

// orgDataQuery fetches a page of non-archived repos and the first page of each
// repo's open PRs for an organization. Repos with more than 100 open PRs are
// completed by repoPRsQuery (see fetchRemainingPRs).
const orgDataQuery = `
query($org: String!, $repoCursor: String) {
  organization(login: $org) {
    repositories(first: 100, after: $repoCursor, isArchived: false, orderBy: {field: PUSHED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        name
        nameWithOwner
        url
        isDisabled
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
          nodes {` + prFields + `}
        }
      }
    }
  }
}
`

// repoPRsQuery paginates the open PRs of a single repository, used to fetch any
// PRs beyond the first 100 returned by orgDataQuery.
const repoPRsQuery = `
query($owner: String!, $name: String!, $prCursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: 100, after: $prCursor, states: OPEN, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {` + prFields + `}
    }
  }
}
`

// GraphQL response types for unmarshaling.

type gqlResponse struct {
	Data struct {
		Organization struct {
			Repositories struct {
				PageInfo gqlPageInfo `json:"pageInfo"`
				Nodes    []gqlRepo   `json:"nodes"`
			} `json:"repositories"`
		} `json:"organization"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type gqlPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type gqlRepo struct {
	Name          string  `json:"name"`
	NameWithOwner string  `json:"nameWithOwner"`
	URL           string  `json:"url"`
	IsDisabled    bool    `json:"isDisabled"`
	PushedAt      *string `json:"pushedAt"`
	Owner         struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatarUrl"`
		URL       string `json:"url"`
	} `json:"owner"`
	DefaultBranchRef *struct {
		Name   string `json:"name"`
		Target struct {
			StatusCheckRollup *struct {
				State string `json:"state"`
			} `json:"statusCheckRollup"`
		} `json:"target"`
	} `json:"defaultBranchRef"`
	PullRequests struct {
		PageInfo gqlPageInfo `json:"pageInfo"`
		Nodes    []gqlPR     `json:"nodes"`
	} `json:"pullRequests"`
}

type gqlPR struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	IsDraft   bool   `json:"isDraft"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Mergeable string `json:"mergeable"`
	Author    *struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatarUrl"`
		URL       string `json:"url"`
	} `json:"author"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	HeadRefOid  string `json:"headRefOid"`
	Labels      struct {
		Nodes []struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		} `json:"nodes"`
	} `json:"labels"`
	ReviewRequests struct {
		TotalCount int `json:"totalCount"`
	} `json:"reviewRequests"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// repoPRsResponse unmarshals the per-repo PR pagination query (repoPRsQuery).
type repoPRsResponse struct {
	Data struct {
		Repository struct {
			PullRequests struct {
				PageInfo gqlPageInfo `json:"pageInfo"`
				Nodes    []gqlPR     `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// OrgData holds the fetched repos and their PRs keyed by "owner/repo".
type OrgData struct {
	Repos      []dbgen.Repo
	PRsByRepo  map[string][]dbgen.PullRequest       // key: "owner/repo"
	LabelsByPR map[string]map[int64][]dbgen.PrLabel // key: "owner/repo" -> pr number -> labels
}

// GetOrgData fetches all non-archived repos and open PRs for an org via GraphQL.
// Handles pagination for repositories.
func (c *Client) GetOrgData(ctx context.Context, orgLogin string) (*OrgData, error) {
	result := &OrgData{
		PRsByRepo:  make(map[string][]dbgen.PullRequest),
		LabelsByPR: make(map[string]map[int64][]dbgen.PrLabel),
	}

	var repoCursor *string
	for {
		vars := map[string]interface{}{
			"org": orgLogin,
		}
		if repoCursor != nil {
			vars["repoCursor"] = *repoCursor
		}

		body, err := json.Marshal(map[string]interface{}{
			"query":     orgDataQuery,
			"variables": vars,
		})
		if err != nil {
			return nil, err
		}

		var resp gqlResponse
		if err := c.doJSON(ctx, "POST", "/graphql", bytes.NewReader(body), &resp); err != nil {
			return nil, err
		}
		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
		}

		repos := resp.Data.Organization.Repositories
		for _, gr := range repos.Nodes {
			repo := convertRepo(orgLogin, gr)
			result.Repos = append(result.Repos, repo)

			repoKey := repo.NameWithOwner
			for _, gpr := range gr.PullRequests.Nodes {
				addPR(result, orgLogin, gr.Name, repoKey, gpr)
			}

			// Repos with more than 100 open PRs need follow-up pages.
			if gr.PullRequests.PageInfo.HasNextPage {
				if err := c.fetchRemainingPRs(ctx, result, orgLogin, gr.Name, repoKey, gr.PullRequests.PageInfo.EndCursor); err != nil {
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

// fetchRemainingPRs pages through a single repo's remaining open PRs, starting
// after startCursor, appending them into result.
func (c *Client) fetchRemainingPRs(ctx context.Context, result *OrgData, orgLogin, repoName, repoKey, startCursor string) error {
	cursor := startCursor
	for {
		body, err := json.Marshal(map[string]interface{}{
			"query": repoPRsQuery,
			"variables": map[string]interface{}{
				"owner":    orgLogin,
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
			addPR(result, orgLogin, repoName, repoKey, gpr)
		}

		if !prs.PageInfo.HasNextPage {
			return nil
		}
		cursor = prs.PageInfo.EndCursor
	}
}

// addPR converts and appends a PR (and its labels) into result under repoKey.
func addPR(result *OrgData, orgLogin, repoName, repoKey string, gpr gqlPR) {
	pr := convertPR(orgLogin, repoName, gpr)
	result.PRsByRepo[repoKey] = append(result.PRsByRepo[repoKey], pr)

	if len(gpr.Labels.Nodes) == 0 {
		return
	}
	if result.LabelsByPR[repoKey] == nil {
		result.LabelsByPR[repoKey] = make(map[int64][]dbgen.PrLabel)
	}
	for _, l := range gpr.Labels.Nodes {
		result.LabelsByPR[repoKey][int64(gpr.Number)] = append(
			result.LabelsByPR[repoKey][int64(gpr.Number)],
			dbgen.PrLabel{
				Owner:    orgLogin,
				Repo:     repoName,
				PrNumber: int64(gpr.Number),
				Name:     l.Name,
				Color:    l.Color,
			},
		)
	}
}

func convertRepo(owner string, gr gqlRepo) dbgen.Repo {
	r := dbgen.Repo{
		Owner:         owner,
		Name:          gr.Name,
		NameWithOwner: gr.NameWithOwner,
		Url:           gr.URL,
		IsDisabled:    boolToInt(gr.IsDisabled),
		OwnerLogin:    sql.NullString{String: gr.Owner.Login, Valid: gr.Owner.Login != ""},
		OwnerAvatar:   sql.NullString{String: gr.Owner.AvatarURL, Valid: gr.Owner.AvatarURL != ""},
		OwnerUrl:      sql.NullString{String: gr.Owner.URL, Valid: gr.Owner.URL != ""},
	}
	if gr.PushedAt != nil {
		r.PushedAt = sql.NullString{String: *gr.PushedAt, Valid: true}
	}
	if gr.DefaultBranchRef != nil {
		r.DefaultBranch = sql.NullString{String: gr.DefaultBranchRef.Name, Valid: true}
		if gr.DefaultBranchRef.Target.StatusCheckRollup != nil {
			r.DefaultBranchStatus = sql.NullString{
				String: gr.DefaultBranchRef.Target.StatusCheckRollup.State,
				Valid:  true,
			}
		}
	}
	return r
}

func convertPR(owner, repoName string, gpr gqlPR) dbgen.PullRequest {
	pr := dbgen.PullRequest{
		Owner:              owner,
		Repo:               repoName,
		Number:             int64(gpr.Number),
		Title:              gpr.Title,
		Url:                gpr.URL,
		IsDraft:            boolToInt(gpr.IsDraft),
		State:              "OPEN",
		CreatedAt:          gpr.CreatedAt,
		UpdatedAt:          gpr.UpdatedAt,
		Additions:          sql.NullInt64{Int64: int64(gpr.Additions), Valid: true},
		Deletions:          sql.NullInt64{Int64: int64(gpr.Deletions), Valid: true},
		Mergeable:          sql.NullString{String: gpr.Mergeable, Valid: gpr.Mergeable != ""},
		HeadRefName:        sql.NullString{String: gpr.HeadRefName, Valid: gpr.HeadRefName != ""},
		BaseRefName:        sql.NullString{String: gpr.BaseRefName, Valid: gpr.BaseRefName != ""},
		HeadRefOid:         sql.NullString{String: gpr.HeadRefOid, Valid: gpr.HeadRefOid != ""},
		ReviewRequestCount: sql.NullInt64{Int64: int64(gpr.ReviewRequests.TotalCount), Valid: true},
	}
	if gpr.Author != nil {
		pr.AuthorLogin = sql.NullString{String: gpr.Author.Login, Valid: true}
		pr.AuthorAvatar = sql.NullString{String: gpr.Author.AvatarURL, Valid: true}
		pr.AuthorUrl = sql.NullString{String: gpr.Author.URL, Valid: true}
	}
	if len(gpr.Commits.Nodes) > 0 {
		commit := gpr.Commits.Nodes[0]
		if commit.Commit.StatusCheckRollup != nil {
			pr.LastCommitStatus = sql.NullString{
				String: commit.Commit.StatusCheckRollup.State,
				Valid:  true,
			}
		}
	}
	return pr
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
