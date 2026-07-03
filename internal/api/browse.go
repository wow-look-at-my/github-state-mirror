package api

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

// Admin cache browse + consistency check.
//
// These two endpoints are the operator's window into the cache. They are gated
// to admins (the same logins that get the all-scopes view): the cache stays
// partitioned per GitHub user (per token fingerprint for non-user tokens), and
// a normal signed-in user still only ever sees counts for their own scope. An
// admin, who already sees every scope's counts, can additionally read the
// actual rows and diff a scope against GitHub. Nothing here writes to the
// cache. The `actor` query parameter is the full partition key exactly as the
// dashboard's `actor_id` reports it ("user:<id>", a hex fingerprint, or
// "app-installation:<id>").

// ---- clean JSON views of the cached rows ----
//
// The sqlc row types embed sql.NullString/NullInt64, which serialize to
// {"String":...,"Valid":...}. These view structs flatten them so the browse
// payload reads cleanly and is easy to paste back for analysis.

type browseRepo struct {
	Owner               string `json:"owner"`
	Name                string `json:"name"`
	NameWithOwner       string `json:"name_with_owner"`
	URL                 string `json:"url"`
	IsDisabled          bool   `json:"is_disabled"`
	IsArchived          bool   `json:"is_archived"`
	PushedAt            string `json:"pushed_at,omitempty"`
	DefaultBranch       string `json:"default_branch,omitempty"`
	DefaultBranchStatus string `json:"default_branch_status,omitempty"`
}

type browsePR struct {
	Owner            string   `json:"owner"`
	Repo             string   `json:"repo"`
	Number           int64    `json:"number"`
	Title            string   `json:"title"`
	URL              string   `json:"url"`
	State            string   `json:"state"`
	IsDraft          bool     `json:"is_draft"`
	AuthorLogin      string   `json:"author_login,omitempty"`
	BaseRef          string   `json:"base_ref,omitempty"`
	HeadRef          string   `json:"head_ref,omitempty"`
	HeadSHA          string   `json:"head_sha,omitempty"`
	Additions        int64    `json:"additions"`
	Deletions        int64    `json:"deletions"`
	Mergeable        string   `json:"mergeable,omitempty"`
	ReviewRequests   int64    `json:"review_requests"`
	LastCommitStatus string   `json:"last_commit_status,omitempty"`
	Labels           []string `json:"labels,omitempty"`
	CreatedAt        string   `json:"created_at,omitempty"`
	UpdatedAt        string   `json:"updated_at,omitempty"`
}

type browseOrg struct {
	Login string `json:"login"`
	URL   string `json:"url,omitempty"`
}

type browseUser struct {
	Login  string `json:"login"`
	URL    string `json:"url,omitempty"`
	Avatar string `json:"avatar_url,omitempty"`
}

type browseComparison struct {
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	BaseRef  string `json:"base_ref"`
	HeadRef  string `json:"head_ref"`
	AheadBy  int64  `json:"ahead_by"`
	BehindBy int64  `json:"behind_by"`
}

type browsePRFile struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	PRNumber  int64  `json:"pr_number"`
	Path      string `json:"path"`
	Additions int64  `json:"additions"`
	Deletions int64  `json:"deletions"`
}

type browseCommitCheck struct {
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	SHA     string `json:"sha"`
	Context string `json:"context"`
	State   string `json:"state"`
}

type browseResponse struct {
	Actor             string              `json:"actor"`    // short fingerprint (display)
	ActorID           string              `json:"actor_id"` // full partition key
	Login             string              `json:"login,omitempty"`
	Counts            ghdata.DataCounts   `json:"counts"`
	Repos             []browseRepo        `json:"repos"`
	PullRequests      []browsePR          `json:"pull_requests"`
	Orgs              []browseOrg         `json:"orgs"`
	Users             []browseUser        `json:"users"`
	BranchComparisons []browseComparison  `json:"branch_comparisons"`
	PRFiles           []browsePRFile      `json:"pr_files"`
	CommitChecks      []browseCommitCheck `json:"commit_checks"`
}

// handleCacheData dumps the actual cached rows for one scope (admin only).
func (d *dashboard) handleCacheData(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	actorFP := r.URL.Query().Get("actor")
	if actorFP == "" {
		http.Error(w, "missing 'actor' query parameter", http.StatusBadRequest)
		return
	}

	resp, err := d.collectBrowse(r.Context(), actorFP)
	if err != nil {
		slog.Warn("browse cache failed", "actor", shortFingerprint(actorFP), "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

// collectBrowse loads every cached row for one actor and converts it to the
// clean JSON view.
func (d *dashboard) collectBrowse(ctx context.Context, actorFP string) (browseResponse, error) {
	resp := browseResponse{
		Actor:             shortFingerprint(actorFP),
		ActorID:           actorFP,
		Login:             d.loginForActor(ctx, actorFP),
		Repos:             []browseRepo{},
		PullRequests:      []browsePR{},
		Orgs:              []browseOrg{},
		Users:             []browseUser{},
		BranchComparisons: []browseComparison{},
		PRFiles:           []browsePRFile{},
		CommitChecks:      []browseCommitCheck{},
	}

	counts, err := d.store.DataCounts(ctx, actorFP)
	if err != nil {
		return resp, err
	}
	resp.Counts = counts

	repos, err := d.store.ReposByActor(ctx, actorFP)
	if err != nil {
		return resp, err
	}
	for _, r := range repos {
		resp.Repos = append(resp.Repos, toBrowseRepo(r))
	}

	// Labels grouped by owner/repo/number so each PR carries its own.
	labels, err := d.store.PRLabelsByActor(ctx, actorFP)
	if err != nil {
		return resp, err
	}
	labelsByPR := make(map[string][]string)
	for _, l := range labels {
		key := prKey(l.Owner, l.Repo, l.PrNumber)
		labelsByPR[key] = append(labelsByPR[key], l.Name)
	}

	prs, err := d.store.PullRequestsByActor(ctx, actorFP)
	if err != nil {
		return resp, err
	}
	for _, pr := range prs {
		resp.PullRequests = append(resp.PullRequests, toBrowsePR(pr, labelsByPR[prKey(pr.Owner, pr.Repo, pr.Number)]))
	}

	orgs, err := d.store.OrgsByActor(ctx, actorFP)
	if err != nil {
		return resp, err
	}
	for _, o := range orgs {
		resp.Orgs = append(resp.Orgs, browseOrg{Login: o.Login, URL: nullStr(o.Url)})
	}

	users, err := d.store.UsersByActor(ctx, actorFP)
	if err != nil {
		return resp, err
	}
	for _, u := range users {
		resp.Users = append(resp.Users, browseUser{Login: u.Login, URL: u.Url, Avatar: u.AvatarUrl})
	}

	comparisons, err := d.store.BranchComparisonsByActor(ctx, actorFP)
	if err != nil {
		return resp, err
	}
	for _, c := range comparisons {
		resp.BranchComparisons = append(resp.BranchComparisons, browseComparison{
			Owner: c.Owner, Repo: c.Repo, BaseRef: c.BaseRef, HeadRef: c.HeadRef, AheadBy: c.AheadBy, BehindBy: c.BehindBy,
		})
	}

	files, err := d.store.PRFilesByActor(ctx, actorFP)
	if err != nil {
		return resp, err
	}
	for _, f := range files {
		resp.PRFiles = append(resp.PRFiles, browsePRFile{
			Owner: f.Owner, Repo: f.Repo, PRNumber: f.PrNumber, Path: f.Path, Additions: f.Additions, Deletions: f.Deletions,
		})
	}

	checks, err := d.store.CommitChecksByActor(ctx, actorFP)
	if err != nil {
		return resp, err
	}
	for _, c := range checks {
		resp.CommitChecks = append(resp.CommitChecks, browseCommitCheck{
			Owner: c.Owner, Repo: c.Repo, SHA: c.Sha, Context: c.Context, State: c.State,
		})
	}

	return resp, nil
}

// handleCacheCheck runs the consistency check for one scope (admin only): it
// re-fetches the source of truth from GitHub via the App and returns a JSON diff.
func (d *dashboard) handleCacheCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	if d.checker == nil || !d.checker.Available() {
		http.Error(w, "consistency check unavailable: this server has no GitHub App configured (set GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY)", http.StatusServiceUnavailable)
		return
	}
	actorFP := r.URL.Query().Get("actor")
	if actorFP == "" {
		http.Error(w, "missing 'actor' query parameter", http.StatusBadRequest)
		return
	}
	org := r.URL.Query().Get("org") // optional: limit the check to one owner

	report, err := d.checker.CheckActor(r.Context(), actorFP, d.loginForActor(r.Context(), actorFP), org)
	if err != nil {
		slog.Warn("consistency check failed", "actor", shortFingerprint(actorFP), "error", err)
		http.Error(w, "consistency check failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, report)
}

type rateLimitResponse struct {
	Installations []syncpkg.InstallationRateLimit `json:"installations"`
}

// handleRateLimit reports the GitHub App's rate-limit status per installation
// (admin only). The App is the credential the background fetches and the
// consistency check use, so this is what to watch when fetches start failing.
func (d *dashboard) handleRateLimit(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	if d.checker == nil || !d.checker.Available() {
		http.Error(w, "rate limit unavailable: this server has no GitHub App configured (set GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY)", http.StatusServiceUnavailable)
		return
	}
	limits, err := d.checker.RateLimits(r.Context())
	if err != nil {
		slog.Warn("rate limit fetch failed", "error", err)
		http.Error(w, "rate limit fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if limits == nil {
		limits = []syncpkg.InstallationRateLimit{}
	}
	writeJSON(w, rateLimitResponse{Installations: limits})
}

// requireAdmin enforces a signed-in admin session. It writes the appropriate
// error and returns ok=false when the requester is anonymous or not an admin.
func (d *dashboard) requireAdmin(w http.ResponseWriter, r *http.Request) (login string, ok bool) {
	login, signedIn := d.auth.Session(r)
	if !signedIn {
		http.Error(w, "unauthorized: sign in first", http.StatusUnauthorized)
		return "", false
	}
	if !d.auth.IsAdmin(login) {
		http.Error(w, "forbidden: admin only", http.StatusForbidden)
		return "", false
	}
	return login, true
}

// loginForActor returns the GitHub login recorded for an actor, or "" if none.
func (d *dashboard) loginForActor(ctx context.Context, actorFP string) string {
	identities, err := d.store.ListActorIdentities(ctx)
	if err != nil {
		return ""
	}
	for _, id := range identities {
		if id.Actor == actorFP {
			return id.Login
		}
	}
	return ""
}

// ---- conversion helpers ----

func prKey(owner, repo string, number int64) string {
	return owner + "/" + repo + "#" + strconv.FormatInt(number, 10)
}

func nullStr(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func nullInt(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

func intToBool(v int64) bool { return v != 0 }

func toBrowseRepo(r dbgen.Repo) browseRepo {
	return browseRepo{
		Owner:               r.Owner,
		Name:                r.Name,
		NameWithOwner:       r.NameWithOwner,
		URL:                 r.Url,
		IsDisabled:          intToBool(r.IsDisabled),
		IsArchived:          intToBool(r.IsArchived),
		PushedAt:            nullStr(r.PushedAt),
		DefaultBranch:       nullStr(r.DefaultBranch),
		DefaultBranchStatus: nullStr(r.DefaultBranchStatus),
	}
}

func toBrowsePR(pr dbgen.PullRequest, labels []string) browsePR {
	return browsePR{
		Owner:            pr.Owner,
		Repo:             pr.Repo,
		Number:           pr.Number,
		Title:            pr.Title,
		URL:              pr.Url,
		State:            pr.State,
		IsDraft:          intToBool(pr.IsDraft),
		AuthorLogin:      nullStr(pr.AuthorLogin),
		BaseRef:          nullStr(pr.BaseRefName),
		HeadRef:          nullStr(pr.HeadRefName),
		HeadSHA:          nullStr(pr.HeadRefOid),
		Additions:        nullInt(pr.Additions),
		Deletions:        nullInt(pr.Deletions),
		Mergeable:        nullStr(pr.Mergeable),
		ReviewRequests:   nullInt(pr.ReviewRequestCount),
		LastCommitStatus: nullStr(pr.LastCommitStatus),
		Labels:           labels,
		CreatedAt:        pr.CreatedAt,
		UpdatedAt:        pr.UpdatedAt,
	}
}
