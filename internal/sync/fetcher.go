package sync

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// UserFetcher fetches the authenticated user. Key: "self".
type UserFetcher struct {
	gh    *ghclient.Client
	store *ghdata.Store
}

func (f *UserFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	user, err := f.gh.GetAuthenticatedUser(ctx)
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	if err := f.store.UpsertUser(ctx, user); err != nil {
		return freshness.RefreshResult{}, err
	}
	return freshness.RefreshResult{RecordsChanged: 1}, nil
}

// UserOrgsFetcher fetches the authenticated user's orgs. Key: "<user_login>".
type UserOrgsFetcher struct {
	gh    *ghclient.Client
	store *ghdata.Store
}

func (f *UserOrgsFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	orgs, err := f.gh.GetUserOrgs(ctx)
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	if err := f.store.SetUserOrgs(ctx, key, orgs); err != nil {
		return freshness.RefreshResult{}, err
	}
	return freshness.RefreshResult{RecordsChanged: len(orgs)}, nil
}

// OrgReposFetcher fetches all repos + open PRs for an org via GraphQL. Key: org login.
type OrgReposFetcher struct {
	gh    *ghclient.Client
	store *ghdata.Store
}

func (f *OrgReposFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	data, err := f.gh.GetOrgData(ctx, key)
	if err != nil {
		return freshness.RefreshResult{}, err
	}

	changed := 0

	if err := f.store.SetOrgRepos(ctx, key, data.Repos); err != nil {
		return freshness.RefreshResult{}, err
	}
	changed += len(data.Repos)

	for _, repo := range data.Repos {
		repoKey := repo.NameWithOwner
		prs := data.PRsByRepo[repoKey]
		labels := data.LabelsByPR[repoKey]
		if err := f.store.SetRepoPRs(ctx, key, repo.Name, prs, labels); err != nil {
			return freshness.RefreshResult{}, err
		}
		changed += len(prs)
	}

	return freshness.RefreshResult{RecordsChanged: changed}, nil
}

// PRFilesFetcher fetches files for a PR. Key: "owner/repo/number".
type PRFilesFetcher struct {
	gh    *ghclient.Client
	store *ghdata.Store
}

func (f *PRFilesFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	owner, repo, number, err := parseOwnerRepoNumber(key)
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	files, err := f.gh.GetPRFiles(ctx, owner, repo, number)
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	if err := f.store.SetPRFiles(ctx, owner, repo, number, files); err != nil {
		return freshness.RefreshResult{}, err
	}
	return freshness.RefreshResult{RecordsChanged: len(files)}, nil
}

// CompareFetcher fetches branch comparison. Key: "owner/repo/base...head".
type CompareFetcher struct {
	gh    *ghclient.Client
	store *ghdata.Store
}

func (f *CompareFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	owner, repo, base, head, err := parseCompareKey(key)
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	comp, err := f.gh.CompareBranches(ctx, owner, repo, base, head)
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	if err := f.store.UpsertComparison(ctx, comp); err != nil {
		return freshness.RefreshResult{}, err
	}
	return freshness.RefreshResult{RecordsChanged: 1}, nil
}

// Key parsers

func parseOwnerRepoNumber(key string) (owner, repo string, number int64, err error) {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) != 3 {
		return "", "", 0, fmt.Errorf("invalid pr_files key: %q", key)
	}
	n, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid pr number in key: %q", key)
	}
	return parts[0], parts[1], n, nil
}

func parseCompareKey(key string) (owner, repo, base, head string, err error) {
	// key format: "owner/repo/base...head"
	parts := strings.SplitN(key, "/", 3)
	if len(parts) != 3 {
		return "", "", "", "", fmt.Errorf("invalid compare key: %q", key)
	}
	refs := strings.SplitN(parts[2], "...", 2)
	if len(refs) != 2 {
		return "", "", "", "", fmt.Errorf("invalid compare refs in key: %q", key)
	}
	return parts[0], parts[1], refs[0], refs[1], nil
}
