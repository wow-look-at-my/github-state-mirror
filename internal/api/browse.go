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
// These endpoints are the operator's window into the ONE global truth store
// and the reveal layer. They are gated to admins: the reveal layer filters
// what data-API callers see, but the operator's dashboard deliberately sees
// everything (it is the surface for diagnosing the cache itself). Nothing
// here writes to the cache.
//
//	GET /api/cache/data                     -- dump global truth rows
//	GET /api/cache/data?principal=<id>      -- one principal's grants
//	GET /api/cache/check[?org=<owner>]      -- diff global truth vs GitHub

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
	Visibility          string `json:"visibility,omitempty"` // '' = unknown (treated private)
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
	TouchedAt        string   `json:"touched_at,omitempty"`
	RestComplete     bool     `json:"rest_complete"`
}

type browseCommitCheck struct {
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	SHA     string `json:"sha"`
	Context string `json:"context"`
	State   string `json:"state"`
}

type browseGrant struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	Source    string `json:"source"`
	GrantedAt string `json:"granted_at"`
	ExpiresAt string `json:"expires_at"`
}

type browseResponse struct {
	Counts       ghdata.DataCounts   `json:"counts"`
	Repos        []browseRepo        `json:"repos"`
	PullRequests []browsePR          `json:"pull_requests"`
	CommitChecks []browseCommitCheck `json:"commit_checks"`
}

type grantsResponse struct {
	Principal   string        `json:"principal"`    // short (display)
	PrincipalID string        `json:"principal_id"` // full key
	Login       string        `json:"login,omitempty"`
	Grants      []browseGrant `json:"grants"`
}

// handleCacheData dumps the global truth rows (admin only). With ?principal=
// it instead dumps that principal's grants -- who can see what.
func (d *dashboard) handleCacheData(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	if principal := r.URL.Query().Get("principal"); principal != "" {
		d.serveGrants(w, r, principal)
		return
	}

	resp, err := d.collectBrowse(r.Context())
	if err != nil {
		slog.Warn("browse cache failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

// serveGrants dumps one principal's grants (the reveal layer's answer to "what
// can this principal see?").
func (d *dashboard) serveGrants(w http.ResponseWriter, r *http.Request, principal string) {
	rows, err := d.store.GrantsByPrincipal(r.Context(), principal)
	if err != nil {
		slog.Warn("browse grants failed", "principal", shortFingerprint(principal), "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	resp := grantsResponse{
		Principal:   shortFingerprint(principal),
		PrincipalID: principal,
		Login:       d.loginForActor(r.Context(), principal),
		Grants:      make([]browseGrant, 0, len(rows)),
	}
	for _, g := range rows {
		resp.Grants = append(resp.Grants, browseGrant{
			Owner: g.Owner, Repo: g.Repo, Source: g.Source,
			GrantedAt: g.GrantedAt, ExpiresAt: g.ExpiresAt,
		})
	}
	writeJSON(w, resp)
}

// collectBrowse loads the global truth rows and converts them to the clean
// JSON view.
func (d *dashboard) collectBrowse(ctx context.Context) (browseResponse, error) {
	resp := browseResponse{
		Repos:        []browseRepo{},
		PullRequests: []browsePR{},
		CommitChecks: []browseCommitCheck{},
	}

	counts, err := d.store.GlobalDataCounts(ctx)
	if err != nil {
		return resp, err
	}
	resp.Counts = counts

	repos, err := d.store.AllRepos(ctx)
	if err != nil {
		return resp, err
	}
	for _, r := range repos {
		resp.Repos = append(resp.Repos, toBrowseRepo(r))
	}

	// Labels grouped by owner/repo/number so each PR carries its own.
	labels, err := d.store.AllPRLabels(ctx)
	if err != nil {
		return resp, err
	}
	labelsByPR := make(map[string][]string)
	for _, l := range labels {
		key := prKey(l.Owner, l.Repo, l.PrNumber)
		labelsByPR[key] = append(labelsByPR[key], l.Name)
	}

	prs, err := d.store.AllPullRequests(ctx)
	if err != nil {
		return resp, err
	}
	for _, pr := range prs {
		resp.PullRequests = append(resp.PullRequests, toBrowsePR(pr, labelsByPR[prKey(pr.Owner, pr.Repo, pr.Number)]))
	}

	checks, err := d.store.AllCommitChecks(ctx)
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

// handleCacheCheck runs the consistency check (admin only): it re-fetches the
// source of truth from GitHub via the App and returns a JSON diff of GLOBAL
// truth vs GitHub. Optional ?org= limits the check to one owner.
func (d *dashboard) handleCacheCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	if d.checker == nil || !d.checker.Available() {
		http.Error(w, "consistency check unavailable: this server has no GitHub App configured (set GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY)", http.StatusServiceUnavailable)
		return
	}
	org := r.URL.Query().Get("org") // optional: limit the check to one owner

	report, err := d.checker.Check(r.Context(), org)
	if err != nil {
		slog.Warn("consistency check failed", "error", err)
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

// loginForActor returns the GitHub login recorded for a principal, or "" if none.
func (d *dashboard) loginForActor(ctx context.Context, principal string) string {
	identities, err := d.store.ListActorIdentities(ctx)
	if err != nil {
		return ""
	}
	for _, id := range identities {
		if id.Actor == principal {
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
		Visibility:          r.Visibility,
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
		TouchedAt:        pr.TouchedAt,
		RestComplete:     ghdata.PRRestComplete(pr),
	}
}
