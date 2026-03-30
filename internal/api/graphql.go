package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

// graphql handles the POST /graphql endpoint.
// It extracts the org name from the query variables, ensures freshness,
// then assembles a response matching the expected GraphQL shape.
func (h *handlers) graphql(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Extract org from variables.
	orgLogin, _ := req.Variables["org"].(string)
	if orgLogin == "" {
		// Try to extract from the query itself (fallback).
		orgLogin = extractOrgFromQuery(req.Query)
	}
	if orgLogin == "" {
		http.Error(w, "missing org variable", http.StatusBadRequest)
		return
	}

	// Ensure org repos data is fresh.
	if err := h.mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: syncpkg.KindOrgRepos, Key: orgLogin}); err != nil {
		slog.Warn("ensure fresh org repos failed", "org", orgLogin, "error", err)
	}

	// Read repos from store.
	repos, err := h.store.ListReposByOwner(ctx, orgLogin)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Build response matching GraphQL shape.
	repoNodes := make([]map[string]interface{}, 0, len(repos))
	for _, repo := range repos {
		// Get open PRs for this repo.
		prs, err := h.store.ListOpenPRsByRepo(ctx, repo.Owner, repo.Name)
		if err != nil {
			slog.Warn("list open prs failed", "repo", repo.NameWithOwner, "error", err)
			continue
		}

		prNodes := make([]map[string]interface{}, 0, len(prs))
		for _, pr := range prs {
			// Get labels for this PR.
			labels, _ := h.store.ListPRLabels(ctx, pr.Owner, pr.Repo, pr.Number)
			labelNodes := make([]map[string]interface{}, 0, len(labels))
			for _, l := range labels {
				labelNodes = append(labelNodes, map[string]interface{}{
					"name":  l.Name,
					"color": l.Color,
				})
			}

			var lastCommitStatus interface{}
			if pr.LastCommitStatus.Valid {
				lastCommitStatus = map[string]interface{}{
					"nodes": []map[string]interface{}{
						{
							"commit": map[string]interface{}{
								"statusCheckRollup": map[string]interface{}{
									"state": pr.LastCommitStatus.String,
								},
							},
						},
					},
				}
			}

			prNode := map[string]interface{}{
				"number":    pr.Number,
				"title":     pr.Title,
				"url":       pr.Url,
				"isDraft":   pr.IsDraft != 0,
				"createdAt": pr.CreatedAt,
				"updatedAt": pr.UpdatedAt,
				"additions": pr.Additions.Int64,
				"deletions": pr.Deletions.Int64,
				"mergeable": pr.Mergeable.String,
				"author": map[string]interface{}{
					"login":     pr.AuthorLogin.String,
					"avatarUrl": pr.AuthorAvatar.String,
					"url":       pr.AuthorUrl.String,
				},
				"headRefName": pr.HeadRefName.String,
				"baseRefName": pr.BaseRefName.String,
				"headRefOid":  pr.HeadRefOid.String,
				"labels": map[string]interface{}{
					"nodes": labelNodes,
				},
				"reviewRequests": map[string]interface{}{
					"totalCount": pr.ReviewRequestCount.Int64,
				},
				"commits": lastCommitStatus,
			}
			prNodes = append(prNodes, prNode)
		}

		var defaultBranchRef interface{}
		if repo.DefaultBranch.Valid {
			branchRef := map[string]interface{}{
				"name": repo.DefaultBranch.String,
			}
			if repo.DefaultBranchStatus.Valid {
				branchRef["target"] = map[string]interface{}{
					"statusCheckRollup": map[string]interface{}{
						"state": repo.DefaultBranchStatus.String,
					},
				}
			}
			defaultBranchRef = branchRef
		}

		repoNode := map[string]interface{}{
			"name":          repo.Name,
			"nameWithOwner": repo.NameWithOwner,
			"url":           repo.Url,
			"isDisabled":    repo.IsDisabled != 0,
			"pushedAt":      repo.PushedAt.String,
			"owner": map[string]interface{}{
				"login":     repo.OwnerLogin.String,
				"avatarUrl": repo.OwnerAvatar.String,
				"url":       repo.OwnerUrl.String,
			},
			"defaultBranchRef": defaultBranchRef,
			"pullRequests": map[string]interface{}{
				"nodes": prNodes,
			},
		}
		repoNodes = append(repoNodes, repoNode)
	}

	response := map[string]interface{}{
		"data": map[string]interface{}{
			"organization": map[string]interface{}{
				"repositories": map[string]interface{}{
					"nodes": repoNodes,
				},
			},
		},
	}

	writeJSON(w, response)
}


func extractOrgFromQuery(query string) string {
	// Simple heuristic: look for organization(login: "xxx")
	idx := strings.Index(query, `organization(login:`)
	if idx < 0 {
		return ""
	}
	rest := query[idx:]
	q1 := strings.IndexByte(rest, '"')
	if q1 < 0 {
		return ""
	}
	rest = rest[q1+1:]
	q2 := strings.IndexByte(rest, '"')
	if q2 < 0 {
		return ""
	}
	return rest[:q2]
}
