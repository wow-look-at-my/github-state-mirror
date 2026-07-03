package sync

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghjson"
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

// PullRequestRawFetcher fetches the REST response body for one PR and stores
// the URL-stripped JSON shape.
// Key: "owner/repo/number".
type PullRequestRawFetcher struct {
	gh    *ghclient.Client
	store *ghdata.Store
}

func NewPullRequestRawFetcher(gh *ghclient.Client, store *ghdata.Store) freshness.Fetcher {
	return &PullRequestRawFetcher{gh: gh, store: store}
}

func (f *PullRequestRawFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	owner, repo, number, err := parseOwnerRepoNumber(key)
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	resp, err := f.gh.GetREST(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), number))
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	if err := f.store.UpsertRESTResponse(ctx, normalizedRESTResponse(KindPullRequestRaw, key, resp)); err != nil {
		return freshness.RefreshResult{}, err
	}
	return freshness.RefreshResult{RecordsChanged: 1}, nil
}

// RepoContentsFetcher fetches the REST response body for one contents path and
// stores the URL-stripped JSON shape.
// Key: RepoContentsKey(owner, repo, path, rawQuery).
type RepoContentsFetcher struct {
	gh    *ghclient.Client
	store *ghdata.Store
}

func NewRepoContentsFetcher(gh *ghclient.Client, store *ghdata.Store) freshness.Fetcher {
	return &RepoContentsFetcher{gh: gh, store: store}
}

func (f *RepoContentsFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	owner, repo, path, rawQuery, err := parseRepoContentsKey(key)
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	resp, err := f.gh.GetREST(ctx, repoContentsAPIPath(owner, repo, path, rawQuery))
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	if err := f.store.UpsertRESTResponse(ctx, normalizedRESTResponse(KindRepoContents, key, resp)); err != nil {
		return freshness.RefreshResult{}, err
	}
	return freshness.RefreshResult{RecordsChanged: 1}, nil
}

// RepoPullListFetcher fetches one REST PR-list page/query and stores the
// URL-stripped JSON shape.
// Key: RepoPullListKey(owner, repo, accept, apiVersion, rawQuery).
type RepoPullListFetcher struct {
	gh    *ghclient.Client
	store *ghdata.Store
}

func NewRepoPullListFetcher(gh *ghclient.Client, store *ghdata.Store) freshness.Fetcher {
	return &RepoPullListFetcher{gh: gh, store: store}
}

func (f *RepoPullListFetcher) Fetch(ctx context.Context, key string, etag string) (freshness.RefreshResult, error) {
	owner, repo, accept, apiVersion, rawQuery, err := parseRepoPullListKey(key)
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(repo))
	if rawQuery != "" {
		path += "?" + rawQuery
	}
	headers := http.Header{}
	if accept != "" {
		headers.Set("Accept", accept)
	}
	if apiVersion != "" {
		headers.Set("X-GitHub-Api-Version", apiVersion)
	}
	resp, err := f.gh.GetRESTWithHeaders(ctx, path, headers)
	if err != nil {
		return freshness.RefreshResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return freshness.RefreshResult{}, fmt.Errorf("github api GET %s: %d %s", path, resp.StatusCode, string(resp.Body))
	}
	if err := f.store.UpsertRESTResponse(ctx, normalizedRESTResponse(KindRepoPullList, key, resp)); err != nil {
		return freshness.RefreshResult{}, err
	}
	return freshness.RefreshResult{RecordsChanged: 1}, nil
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

func PullRequestKey(owner, repo string, number int64) string {
	return fmt.Sprintf("%s/%s/%d", owner, repo, number)
}

func RepoPullListKey(owner, repo, accept, apiVersion, rawQuery string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return RepoPullListKeyPrefix(owner, repo) + strings.Join([]string{
		enc([]byte(accept)),
		enc([]byte(apiVersion)),
		enc([]byte(rawQuery)),
	}, "|")
}

func RepoPullListKeyPrefix(owner, repo string) string {
	return owner + "/" + repo + "|"
}

func RepoContentsKey(owner, repo, path, rawQuery string) string {
	key := owner + "/" + repo + "/contents/" + strings.TrimPrefix(path, "/")
	if rawQuery == "" {
		return key
	}
	return key + "?" + rawQuery
}

func RepoContentsPathKeyPrefix(owner, repo, path string) string {
	return RepoContentsKey(owner, repo, path, "")
}

func RepoContentsRepoKeyPrefix(owner, repo string) string {
	return owner + "/" + repo + "/contents/"
}

func parseRepoContentsKey(key string) (owner, repo, path, rawQuery string, err error) {
	parts := strings.SplitN(key, "/", 4)
	if len(parts) != 4 || parts[2] != "contents" {
		return "", "", "", "", fmt.Errorf("invalid repo contents key: %q", key)
	}
	path, rawQuery, _ = strings.Cut(parts[3], "?")
	return parts[0], parts[1], path, rawQuery, nil
}

func parseRepoPullListKey(key string) (owner, repo, accept, apiVersion, rawQuery string, err error) {
	prefix, encoded, ok := strings.Cut(key, "|")
	if !ok {
		return "", "", "", "", "", fmt.Errorf("invalid repo pull list key: %q", key)
	}
	parts := strings.SplitN(prefix, "/", 2)
	if len(parts) != 2 {
		return "", "", "", "", "", fmt.Errorf("invalid repo pull list repo key: %q", key)
	}
	fields := strings.Split(encoded, "|")
	if len(fields) != 3 {
		return "", "", "", "", "", fmt.Errorf("invalid repo pull list query key: %q", key)
	}
	dec := func(s string) (string, error) {
		b, err := base64.RawURLEncoding.DecodeString(s)
		return string(b), err
	}
	accept, err = dec(fields[0])
	if err != nil {
		return "", "", "", "", "", err
	}
	apiVersion, err = dec(fields[1])
	if err != nil {
		return "", "", "", "", "", err
	}
	rawQuery, err = dec(fields[2])
	if err != nil {
		return "", "", "", "", "", err
	}
	return parts[0], parts[1], accept, apiVersion, rawQuery, nil
}

func normalizedRESTResponse(kind, key string, resp ghclient.RESTResponse) ghdata.RESTResponse {
	body := resp.Body
	if stripped, err := ghjson.StripURLFields(resp.Body); err == nil {
		body = stripped
	}
	return restResponse(kind, key, resp, body)
}

func restResponse(kind, key string, resp ghclient.RESTResponse, body []byte) ghdata.RESTResponse {
	return ghdata.RESTResponse{
		ResourceKind: kind,
		ResourceKey:  key,
		StatusCode:   int64(resp.StatusCode),
		ContentType:  sql.NullString{String: resp.ContentType, Valid: resp.ContentType != ""},
		Body:         body,
	}
}

func repoContentsAPIPath(owner, repo, path, rawQuery string) string {
	apiPath := fmt.Sprintf("/repos/%s/%s/contents", url.PathEscape(owner), url.PathEscape(repo))
	if path != "" {
		apiPath += "/" + escapePathPreservingSlashes(path)
	}
	if rawQuery != "" {
		apiPath += "?" + rawQuery
	}
	return apiPath
}

func escapePathPreservingSlashes(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

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
