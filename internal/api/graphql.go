package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

// maxGraphQLBodyBytes caps how much of a GraphQL request body we buffer. Real
// GraphQL payloads are tiny; this only guards memory when forwarding arbitrary
// queries to GitHub.
const maxGraphQLBodyBytes = 10 << 20 // 10 MiB

// graphql handles the POST /graphql endpoint.
//
// The mirror only serves the org-repos query shape from its cache. It extracts
// the org from the query variables (or the query text), ensures freshness, and
// assembles a response matching that shape. Any other GraphQL query — a viewer
// query, a repo query, a different org field — is an "unknown" the cache cannot
// answer, so it is forwarded to GitHub uncached.
func (h *handlers) graphql(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Buffer the body so we can both inspect the query and, if we cannot serve
	// it from cache, replay it to GitHub.
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxGraphQLBodyBytes+1))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(bodyBytes) > maxGraphQLBodyBytes {
		http.Error(w, "request entity too large", http.StatusRequestEntityTooLarge)
		return
	}

	var req struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Extract org from variables, falling back to the query text.
	orgLogin, _ := req.Variables["org"].(string)
	if orgLogin == "" {
		orgLogin = extractOrgFromQuery(req.Query)
	}

	// Only the org-repos query shape is served from cache (it names an org and
	// asks for repositories). Forward anything else straight to GitHub, uncached,
	// restoring the body we consumed above. A forwarded MUTATION is recorded as
	// a write (it was proxied because it mutates, not because caching failed);
	// a forwarded query keeps the passthrough label.
	if orgLogin == "" || !strings.Contains(req.Query, "repositories") {
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		r.ContentLength = int64(len(bodyBytes))
		if strings.HasPrefix(strings.TrimSpace(req.Query), "mutation") {
			r = r.WithContext(withDispositionHint(r.Context(), DispWrite))
		} else {
			r = r.WithContext(withDispositionHint(r.Context(), DispPassthrough))
		}
		h.ghProxy.ServeHTTP(w, r)
		return
	}

	// Ensure org repos data is fresh, recording whether this was a cache hit
	// (served fresh) or a miss (triggered a fetch) for the dashboard.
	outcome, ensureErr := h.mgr.EnsureFreshOutcome(ctx, freshness.ResourceID{Kind: syncpkg.KindOrgRepos, Key: orgLogin})
	if ensureErr != nil {
		slog.Warn("ensure fresh org repos failed; serving stale cache if available",
			"org", orgLogin, "actor", actor.FromContext(ctx), "error", ensureErr)
	}
	disp := DispHit
	switch {
	case ensureErr != nil:
		disp = DispError
	case outcome == freshness.OutcomeMiss:
		disp = DispMiss
	}
	h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, disp)

	// Read repos from GLOBAL truth, filtered to what the reveal layer permits
	// this caller: public repos plus the caller's granted repos. The grant set
	// was replace-synced by the caller's own fetch (this request's, or an
	// earlier one within the marker TTL), so the filtered view tracks what
	// GitHub itself answers this principal -- never the whole truth store.
	repos, err := h.store.ListVisibleReposByOwner(ctx, orgLogin, actor.FromContext(ctx), time.Now())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// If the refresh failed and there is no cached data to fall back on, surface
	// the upstream error instead of returning an empty-but-"200 OK" success — a
	// silent empty result is indistinguishable from "this org has no repos" and
	// hides real failures (bad token, GitHub 5xx, rate limit, ...). When cached
	// repos DO exist we serve them (stale is better than an error). The body is a
	// GitHub-style GraphQL error envelope so clients can read errors[].message.
	if ensureErr != nil && len(repos) == 0 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{"organization": nil},
			"errors": []map[string]interface{}{{
				"message": "github-state-mirror: failed to fetch repositories for organization " + orgLogin + ": " + ensureErr.Error(),
				"type":    "UPSTREAM_FETCH_FAILED",
			}},
		})
		return
	}

	// Serving stale-on-error: flag it in response HEADERS so clients can tell
	// (and see how old the data is). Headers only — the body must stay
	// byte-identical to GitHub's shape (the identity-test contract).
	if ensureErr != nil {
		w.Header().Set("X-GSM-Stale", "true")
		if meta, merr := h.mgr.Metadata(ctx, freshness.ResourceID{Kind: syncpkg.KindOrgRepos, Key: orgLogin}); merr == nil && meta != nil && meta.LastFetchedAt != nil {
			w.Header().Set("X-GSM-Last-Fetched", meta.LastFetchedAt.UTC().Format(time.RFC3339))
		}
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

			// statusCheckRollup is null when no CI status is recorded, mirroring
			// real GitHub. The commits object itself is always a well-formed node
			// list so clients can safely read commits.nodes[0].commit.statusCheckRollup.
			var rollup interface{}
			if pr.LastCommitStatus.Valid {
				rollup = map[string]interface{}{
					"state": pr.LastCommitStatus.String,
				}
			}
			commits := map[string]interface{}{
				"nodes": []map[string]interface{}{
					{
						"commit": map[string]interface{}{
							"statusCheckRollup": rollup,
						},
					},
				},
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
				"commits": commits,
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
				// The canonical query selects pullRequests.pageInfo (queries.go),
				// so GitHub returns it; include it to stay shape-identical. The
				// mirror returns every open PR in one page, so it never advances.
				"pageInfo": map[string]interface{}{
					"hasNextPage": false,
					"endCursor":   nil,
				},
				"nodes": prNodes,
			},
		}
		repoNodes = append(repoNodes, repoNode)
	}

	response := map[string]interface{}{
		"data": map[string]interface{}{
			"organization": map[string]interface{}{
				"repositories": map[string]interface{}{
					// The mirror returns every repo in one response, so paging
					// always terminates after the first page. pageInfo must be
					// present: clients read pageInfo.hasNextPage unconditionally.
					"pageInfo": map[string]interface{}{
						"hasNextPage": false,
						"endCursor":   nil,
					},
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
