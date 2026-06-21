package sync

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// ConsistencyChecker compares a cache scope against GitHub's live state and
// reports the drift. It fetches the "source of truth" with the mirror's own
// GitHub App (the same credential the periodic refresher uses), so it needs no
// caller token and is a perfect visibility match for the app-installation
// scopes — the webhook-fed buckets where drift is most likely. The cache is
// never modified: this only reads from GitHub and from the store, then diffs.
type ConsistencyChecker struct {
	gh    *ghclient.Client
	store *ghdata.Store
	app   *ghclient.AppAuthenticator // nil when no GitHub App is configured
}

func NewConsistencyChecker(gh *ghclient.Client, store *ghdata.Store, app *ghclient.AppAuthenticator) *ConsistencyChecker {
	return &ConsistencyChecker{gh: gh, store: store, app: app}
}

// Available reports whether the checker can run. The consistency check needs the
// GitHub App to fetch source of truth; without it there is no server-side
// credential able to read repo/PR data (the dashboard's OAuth login is
// read:user only).
func (c *ConsistencyChecker) Available() bool { return c.app != nil }

// InstallationRateLimit is the GitHub rate-limit status for one App installation
// (the credential the background fetches and the consistency check actually use).
type InstallationRateLimit struct {
	Installation string                                `json:"installation"` // account login
	AccountType  string                                `json:"account_type,omitempty"`
	Resources    map[string]ghclient.RateLimitResource `json:"resources,omitempty"`
	Error        string                                `json:"error,omitempty"`
}

// RateLimits reports the GitHub rate-limit status for each App installation, so
// the operator can see how much of the App's quota the background fetches and
// the consistency check are consuming (and when it resets). It mints a
// short-lived installation token per installation and queries GET /rate_limit,
// which does not itself count against the limit.
func (c *ConsistencyChecker) RateLimits(ctx context.Context) ([]InstallationRateLimit, error) {
	if c.app == nil {
		return nil, fmt.Errorf("rate limit unavailable: no GitHub App configured (set GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY)")
	}
	installs, err := c.app.Installations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list GitHub App installations: %w", err)
	}
	out := make([]InstallationRateLimit, 0, len(installs))
	for _, inst := range installs {
		entry := InstallationRateLimit{Installation: inst.Account.Login, AccountType: inst.Account.Type}
		token, err := c.app.InstallationToken(ctx, inst.ID)
		if err != nil {
			entry.Error = "could not mint installation token: " + err.Error()
			out = append(out, entry)
			continue
		}
		rl, err := c.gh.GetRateLimit(ghclient.WithToken(ctx, token))
		if err != nil {
			entry.Error = "fetch /rate_limit failed: " + err.Error()
			out = append(out, entry)
			continue
		}
		entry.Resources = rl.Resources
		out = append(out, entry)
	}
	return out, nil
}

// ConsistencyReport is the full drift report for one cache scope, designed to be
// copy-pasted back for analysis.
type ConsistencyReport struct {
	Scope         string        `json:"scope"`           // short fingerprint (display)
	ScopeFull     string        `json:"scope_full"`      // full actor partition key
	Login         string        `json:"login,omitempty"` // GitHub login for the scope, if known
	FetchedAs     string        `json:"fetched_as"`      // identity used to read GitHub (the truth source)
	GeneratedAt   string        `json:"generated_at"`    // RFC3339
	OrgsChecked   []string      `json:"orgs_checked"`    // owners actually re-fetched and diffed
	OrgsSkipped   []OrgSkip     `json:"orgs_skipped,omitempty"`
	Summary       CheckSummary  `json:"summary"`
	Discrepancies []Discrepancy `json:"discrepancies"`
	Notes         []string      `json:"notes,omitempty"` // caveats to keep in mind when reading the report
}

// OrgSkip records an owner that could not be checked and why (so the absence of
// discrepancies for it is not mistaken for "consistent").
type OrgSkip struct {
	Org    string `json:"org"`
	Reason string `json:"reason"`
}

// CheckSummary is the headline tally.
type CheckSummary struct {
	OrgsChecked       int `json:"orgs_checked"`
	ReposCached       int `json:"repos_cached"`
	OpenPRsCached     int `json:"open_prs_cached"`
	Discrepancies     int `json:"discrepancies"`
	ReposOnlyInCache  int `json:"repos_only_in_cache"`
	ReposOnlyOnGitHub int `json:"repos_only_on_github"`
	PRsOnlyInCache    int `json:"prs_only_in_cache"`
	PRsOnlyOnGitHub   int `json:"prs_only_on_github"`
	FieldMismatches   int `json:"field_mismatches"`
}

// Discrepancy is one difference between the cache and GitHub. cached/github are
// rendered as strings so the report stays flat and pasteable; an empty value
// with issue=only_* means the resource is absent on that side.
type Discrepancy struct {
	Kind   string `json:"kind"`            // "repo" | "pr"
	Repo   string `json:"repo"`            // "owner/name"
	PR     int64  `json:"pr,omitempty"`    // PR number when kind=="pr"
	Issue  string `json:"issue"`           // "only_in_cache" | "only_on_github" | "field_mismatch"
	Field  string `json:"field,omitempty"` // which field differs (issue==field_mismatch)
	Cached string `json:"cached,omitempty"`
	GitHub string `json:"github,omitempty"`
	Note   string `json:"note,omitempty"`
}

// CheckActor runs the consistency check for one cache scope. When orgFilter is
// non-empty only that owner is checked; otherwise every owner with cached repos
// for the scope is checked. The cache is read-only throughout.
func (c *ConsistencyChecker) CheckActor(ctx context.Context, actorFP, login, orgFilter string) (*ConsistencyReport, error) {
	if c.app == nil {
		return nil, fmt.Errorf("consistency check unavailable: no GitHub App configured (set GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY)")
	}

	report := &ConsistencyReport{
		Scope:         shortFP(actorFP),
		ScopeFull:     actorFP,
		Login:         login,
		FetchedAs:     "github-app",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		OrgsChecked:   []string{},
		Discrepancies: []Discrepancy{},
		Notes: []string{
			"Source of truth was fetched as the mirror's GitHub App, which may not see exactly what the token that populated this scope sees. Owners the app is not installed on are skipped (listed under orgs_skipped), not reported as missing.",
			"Only OPEN pull requests are compared (the cache only retains open PRs). A PR shown as only_in_cache is cached as open but is not in GitHub's current open set, i.e. it was likely closed/merged and a webhook was missed.",
		},
	}

	// Load the cached state for this scope once.
	repos, err := c.store.ReposByActor(ctx, actorFP)
	if err != nil {
		return nil, fmt.Errorf("load cached repos: %w", err)
	}
	openPRs, err := c.store.OpenPullRequestsByActor(ctx, actorFP)
	if err != nil {
		return nil, fmt.Errorf("load cached open PRs: %w", err)
	}
	labels, err := c.store.PRLabelsByActor(ctx, actorFP)
	if err != nil {
		return nil, fmt.Errorf("load cached PR labels: %w", err)
	}
	report.Summary.ReposCached = len(repos)
	report.Summary.OpenPRsCached = len(openPRs)

	// Group cached data by owner.
	reposByOwner := groupReposByOwner(repos)
	prsByOwnerRepo := groupPRsByOwnerRepo(openPRs)
	labelsByRepoPR := groupLabelsByRepoPR(labels)

	// Which owners to check.
	owners := sortedOwners(reposByOwner)
	if orgFilter != "" {
		owners = []string{orgFilter}
	}

	// Resolve App installations once so we know which owners are reachable.
	installs, err := c.app.Installations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list GitHub App installations: %w", err)
	}
	byLogin := make(map[string]ghclient.Installation, len(installs))
	for _, in := range installs {
		byLogin[strings.ToLower(in.Account.Login)] = in
	}

	for _, owner := range owners {
		inst, ok := byLogin[strings.ToLower(owner)]
		if !ok {
			report.OrgsSkipped = append(report.OrgsSkipped, OrgSkip{Org: owner, Reason: "no GitHub App installation for this owner (app not installed, or no access)"})
			continue
		}
		if !strings.EqualFold(inst.Account.Type, "Organization") {
			report.OrgsSkipped = append(report.OrgsSkipped, OrgSkip{Org: owner, Reason: "owner is a " + inst.Account.Type + " account; org-repo fetch is not supported for it"})
			continue
		}

		token, err := c.app.InstallationToken(ctx, inst.ID)
		if err != nil {
			report.OrgsSkipped = append(report.OrgsSkipped, OrgSkip{Org: owner, Reason: "could not mint installation token: " + err.Error()})
			continue
		}
		fetchCtx := ghclient.WithToken(ctx, token)
		data, err := c.gh.GetOrgData(fetchCtx, owner)
		if err != nil {
			report.OrgsSkipped = append(report.OrgsSkipped, OrgSkip{Org: owner, Reason: "fetch from GitHub failed: " + err.Error()})
			continue
		}

		report.OrgsChecked = append(report.OrgsChecked, owner)
		c.diffOwner(report, owner, reposByOwner[owner], prsByOwnerRepo, labelsByRepoPR, data)
	}

	// Finalize summary counts.
	report.Summary.OrgsChecked = len(report.OrgsChecked)
	for _, d := range report.Discrepancies {
		switch d.Issue {
		case "only_in_cache":
			if d.Kind == "repo" {
				report.Summary.ReposOnlyInCache++
			} else {
				report.Summary.PRsOnlyInCache++
			}
		case "only_on_github":
			if d.Kind == "repo" {
				report.Summary.ReposOnlyOnGitHub++
			} else {
				report.Summary.PRsOnlyOnGitHub++
			}
		case "field_mismatch":
			report.Summary.FieldMismatches++
		}
	}
	report.Summary.Discrepancies = len(report.Discrepancies)
	return report, nil
}

// diffOwner compares the cached repos/PRs/labels for one owner against the data
// freshly fetched from GitHub, appending discrepancies to the report.
func (c *ConsistencyChecker) diffOwner(
	report *ConsistencyReport,
	owner string,
	cachedRepos map[string]dbgen.Repo,
	cachedPRs map[string]map[int64]dbgen.PullRequest,
	cachedLabels map[string]map[int64]map[string]string,
	data *ghclient.OrgData,
) {
	// --- repos ---
	freshRepos := make(map[string]dbgen.Repo, len(data.Repos))
	for _, r := range data.Repos {
		freshRepos[r.Name] = r
	}
	for name, cr := range cachedRepos {
		fr, ok := freshRepos[name]
		if !ok {
			report.Discrepancies = append(report.Discrepancies, Discrepancy{
				Kind: "repo", Repo: owner + "/" + name, Issue: "only_in_cache",
				Note: "cached but not among GitHub's non-archived repos (archived, deleted, renamed, or no longer visible)",
			})
			continue
		}
		report.Discrepancies = append(report.Discrepancies, repoFieldDiffs(owner, name, cr, fr)...)
	}
	for name, fr := range freshRepos {
		if _, ok := cachedRepos[name]; !ok {
			report.Discrepancies = append(report.Discrepancies, Discrepancy{
				Kind: "repo", Repo: owner + "/" + name, Issue: "only_on_github",
				GitHub: fr.Url,
				Note:   "exists on GitHub but is not cached for this scope",
			})
		}
	}

	// --- pull requests (open only) ---
	for _, fr := range data.Repos {
		repoName := fr.Name
		repoKey := owner + "/" + repoName
		freshPRs := make(map[int64]dbgen.PullRequest)
		for _, pr := range data.PRsByRepo[fr.NameWithOwner] {
			freshPRs[pr.Number] = pr
		}
		cached := cachedPRs[repoKey]

		for num, cpr := range cached {
			fpr, ok := freshPRs[num]
			if !ok {
				report.Discrepancies = append(report.Discrepancies, Discrepancy{
					Kind: "pr", Repo: repoKey, PR: num, Issue: "only_in_cache",
					Cached: cpr.Url,
					Note:   "cached as open but not in GitHub's open PRs (likely closed/merged; a webhook was missed)",
				})
				continue
			}
			report.Discrepancies = append(report.Discrepancies, prFieldDiffs(repoKey, num, cpr, fpr)...)
			report.Discrepancies = append(report.Discrepancies, labelDiffs(repoKey, num,
				cachedLabels[repoKey][num], freshLabelSet(data.LabelsByPR[fr.NameWithOwner][num]))...)
		}
		for num, fpr := range freshPRs {
			if _, ok := cached[num]; !ok {
				report.Discrepancies = append(report.Discrepancies, Discrepancy{
					Kind: "pr", Repo: repoKey, PR: num, Issue: "only_on_github",
					GitHub: fpr.Url,
					Note:   "open on GitHub but not cached for this scope",
				})
			}
		}
	}
}

// repoFieldDiffs compares the webhook-fed / refreshed repo fields.
func repoFieldDiffs(owner, name string, c, g dbgen.Repo) []Discrepancy {
	repoKey := owner + "/" + name
	var out []Discrepancy
	add := func(field, cv, gv string) {
		if cv != gv {
			out = append(out, Discrepancy{Kind: "repo", Repo: repoKey, Issue: "field_mismatch", Field: field, Cached: cv, GitHub: gv})
		}
	}
	add("default_branch_status", ns(c.DefaultBranchStatus), ns(g.DefaultBranchStatus))
	add("default_branch", ns(c.DefaultBranch), ns(g.DefaultBranch))
	add("is_disabled", boolStr(c.IsDisabled), boolStr(g.IsDisabled))
	add("url", c.Url, g.Url)
	return out
}

// prFieldDiffs compares the webhook-fed / refreshed PR fields. created_at and
// updated_at are intentionally not compared (updated_at churns constantly and is
// not a correctness signal).
func prFieldDiffs(repoKey string, num int64, c, g dbgen.PullRequest) []Discrepancy {
	var out []Discrepancy
	add := func(field, cv, gv string) {
		if cv != gv {
			out = append(out, Discrepancy{Kind: "pr", Repo: repoKey, PR: num, Issue: "field_mismatch", Field: field, Cached: cv, GitHub: gv})
		}
	}
	add("title", c.Title, g.Title)
	add("is_draft", boolStr(c.IsDraft), boolStr(g.IsDraft))
	add("last_commit_status", ns(c.LastCommitStatus), ns(g.LastCommitStatus))
	add("mergeable", ns(c.Mergeable), ns(g.Mergeable))
	add("head_ref_oid", ns(c.HeadRefOid), ns(g.HeadRefOid))
	add("head_ref_name", ns(c.HeadRefName), ns(g.HeadRefName))
	add("base_ref_name", ns(c.BaseRefName), ns(g.BaseRefName))
	add("review_request_count", ni(c.ReviewRequestCount), ni(g.ReviewRequestCount))
	return out
}

// labelDiffs compares the cached vs fresh label sets for a PR (name -> color).
func labelDiffs(repoKey string, num int64, cached, fresh map[string]string) []Discrepancy {
	var out []Discrepancy
	for name, cColor := range cached {
		fColor, ok := fresh[name]
		if !ok {
			out = append(out, Discrepancy{Kind: "pr", Repo: repoKey, PR: num, Issue: "field_mismatch", Field: "label:" + name, Cached: cColor, GitHub: "(absent)"})
			continue
		}
		if cColor != fColor {
			out = append(out, Discrepancy{Kind: "pr", Repo: repoKey, PR: num, Issue: "field_mismatch", Field: "label:" + name + " (color)", Cached: cColor, GitHub: fColor})
		}
	}
	for name, fColor := range fresh {
		if _, ok := cached[name]; !ok {
			out = append(out, Discrepancy{Kind: "pr", Repo: repoKey, PR: num, Issue: "field_mismatch", Field: "label:" + name, Cached: "(absent)", GitHub: fColor})
		}
	}
	return out
}

// ---- grouping helpers ----

func groupReposByOwner(repos []dbgen.Repo) map[string]map[string]dbgen.Repo {
	out := make(map[string]map[string]dbgen.Repo)
	for _, r := range repos {
		if out[r.Owner] == nil {
			out[r.Owner] = make(map[string]dbgen.Repo)
		}
		out[r.Owner][r.Name] = r
	}
	return out
}

func groupPRsByOwnerRepo(prs []dbgen.PullRequest) map[string]map[int64]dbgen.PullRequest {
	out := make(map[string]map[int64]dbgen.PullRequest)
	for _, pr := range prs {
		key := pr.Owner + "/" + pr.Repo
		if out[key] == nil {
			out[key] = make(map[int64]dbgen.PullRequest)
		}
		out[key][pr.Number] = pr
	}
	return out
}

func groupLabelsByRepoPR(labels []dbgen.PrLabel) map[string]map[int64]map[string]string {
	out := make(map[string]map[int64]map[string]string)
	for _, l := range labels {
		key := l.Owner + "/" + l.Repo
		if out[key] == nil {
			out[key] = make(map[int64]map[string]string)
		}
		if out[key][l.PrNumber] == nil {
			out[key][l.PrNumber] = make(map[string]string)
		}
		out[key][l.PrNumber][l.Name] = l.Color
	}
	return out
}

func freshLabelSet(labels []dbgen.PrLabel) map[string]string {
	out := make(map[string]string, len(labels))
	for _, l := range labels {
		out[l.Name] = l.Color
	}
	return out
}

func sortedOwners(byOwner map[string]map[string]dbgen.Repo) []string {
	out := make([]string, 0, len(byOwner))
	for o := range byOwner {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// ---- value rendering helpers ----

func ns(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func ni(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return fmt.Sprintf("%d", v.Int64)
}

func boolStr(v int64) string {
	if v != 0 {
		return "true"
	}
	return "false"
}

func shortFP(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}
