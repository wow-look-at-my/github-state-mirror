package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// ConsistencyChecker compares the GLOBAL truth store against GitHub's live
// state and reports the drift -- one comparison for the one cache. It fetches
// the "source of truth" with the mirror's own GitHub App (the same credential
// the periodic refresher uses), via the owner-agnostic repositoryOwner query,
// so User-account installations are checked like Organizations.
//
// Check is strictly read-only. CheckAndApply additionally CORRECTS the drift
// it found: it absorbs the fetched snapshot into truth (SyncOrgTruth under the
// installation's principal), sets visibility/default_branch_status/
// auto_merge_method from GitHub's answers (including nulls the COALESCE
// upserts can never write), and reconciles contradicted commit_checks rows so
// the correction survives the next webhook (see applyOwner).
type ConsistencyChecker struct {
	gh    *ghclient.Client
	store *ghdata.Store
	fresh *freshness.Store           // truth staleness metadata; apply mode also stamps the app-installation marker
	app   *ghclient.AppAuthenticator // nil when no GitHub App is configured
}

func NewConsistencyChecker(gh *ghclient.Client, store *ghdata.Store, fresh *freshness.Store, app *ghclient.AppAuthenticator) *ConsistencyChecker {
	return &ConsistencyChecker{gh: gh, store: store, fresh: fresh, app: app}
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
		// Label the poll with the installation's stable principal (the same
		// key the background refresher runs under), so the passive rate meter
		// records it there instead of under an hourly-rotating token
		// fingerprint — plus the account login as its display name.
		tctx := actor.WithActor(ghclient.WithToken(ctx, token), AppInstallationActor(inst.ID))
		if inst.Account.Login != "" {
			tctx = actor.WithName(tctx, inst.Account.Login)
		}
		rl, err := c.gh.GetRateLimit(tctx)
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

// ConsistencyReport is the full drift report for the global truth store,
// designed to be copy-pasted back for analysis.
type ConsistencyReport struct {
	FetchedAs   string    `json:"fetched_as"`   // identity used to read GitHub (the truth source)
	GeneratedAt string    `json:"generated_at"` // RFC3339
	OrgsChecked []string  `json:"orgs_checked"` // owners actually re-fetched and diffed
	OrgsSkipped []OrgSkip `json:"orgs_skipped,omitempty"`
	// TruthFreshness is, per owner, the most recent org list-sync any
	// principal ran (the fetch that refreshes global truth), so drift can be
	// read against how stale truth actually is.
	TruthFreshness map[string]ScopeFreshness `json:"truth_freshness,omitempty"`
	Summary        CheckSummary              `json:"summary"`
	// Applied tallies apply-mode corrections; nil on a read-only check.
	Applied       *AppliedSummary `json:"applied,omitempty"`
	Discrepancies []Discrepancy   `json:"discrepancies"`
	Notes         []string        `json:"notes,omitempty"` // caveats to keep in mind when reading the report
}

// ScopeFreshness is one owner's most-recent sync metadata.
type ScopeFreshness struct {
	State         string `json:"state"`                     // fresh/stale/fetching/error/unknown
	LastFetchedAt string `json:"last_fetched_at,omitempty"` // RFC3339 of the last successful fetch
	Error         string `json:"error,omitempty"`           // last fetch error, if any
	Principal     string `json:"principal,omitempty"`       // whose sync marker this is
	// PrincipalName is Principal's recorded display name (user login / app
	// slug / installation account login) from actor_identities, when known.
	PrincipalName string `json:"principal_name,omitempty"`
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
	// ReposOnlyOnGitHubPrivate is the subset of ReposOnlyOnGitHub that are
	// private/internal on GitHub -- under the global model these are repos NO
	// principal has synced or webhooked yet (truth is lazy), not per-caller
	// blind spots.
	ReposOnlyOnGitHubPrivate int `json:"repos_only_on_github_private"`
	// ReposOnlyInCacheArchived is the subset of ReposOnlyInCache that are
	// archived (on GitHub or in the cached row): archived repos are excluded
	// from the org data fetch by design, so these are expected, not drift.
	ReposOnlyInCacheArchived int `json:"repos_only_in_cache_archived"`
	PRsOnlyInCache           int `json:"prs_only_in_cache"`
	PRsOnlyOnGitHub          int `json:"prs_only_on_github"`
	FieldMismatches          int `json:"field_mismatches"`
	// VisibilityLeaks counts repos cached PUBLIC that GitHub says are
	// private/internal -- the dangerous direction: the reveal fast path is
	// serving them to any authenticated caller.
	VisibilityLeaks int `json:"visibility_leaks"`
}

// AppliedSummary tallies what apply mode (POST /api/cache/check?apply=true)
// actually corrected, per action.
type AppliedSummary struct {
	ReposAbsorbed          int `json:"repos_absorbed"`
	PRsAbsorbed            int `json:"prs_absorbed"`
	PRsDeleted             int `json:"prs_deleted"`
	VisibilitySet          int `json:"visibility_set"`
	StatusesCorrected      int `json:"statuses_corrected"`
	CheckRowsDeleted       int `json:"check_rows_deleted"`
	DefaultBranchStatusSet int `json:"default_branch_status_set"`
	AutoMergeSet           int `json:"auto_merge_set"`
}

// Discrepancy is one difference between the cache and GitHub. cached/github are
// rendered as strings so the report stays flat and pasteable; an empty value
// with issue=only_* means the resource is absent on that side.
type Discrepancy struct {
	Kind  string `json:"kind"`         // "repo" | "pr"
	Repo  string `json:"repo"`         // "owner/name"
	PR    int64  `json:"pr,omitempty"` // PR number when kind=="pr"
	Issue string `json:"issue"`        // only_in_cache | only_on_github | field_mismatch | visibility_leak | visibility_unknown
	Field string `json:"field,omitempty"`
	// Which field differs (issue==field_mismatch / visibility_*)
	Cached string `json:"cached,omitempty"`
	GitHub string `json:"github,omitempty"`
	// Visibility is the live visibility ("private"/"internal") on an
	// only_on_github repo: global truth simply has not absorbed it yet (no
	// webhook, no principal's sync) -- not necessarily a cache failure.
	Visibility string `json:"visibility,omitempty"`
	// Archived marks an only_in_cache repo whose absence from the org data is
	// explained by archival (expected, not drift).
	Archived bool `json:"archived,omitempty"`
	// Title/UpdatedAt/TouchedAt carry the cached row's detail on
	// pr only_in_cache entries, so the operator can triage without a browse.
	Title     string `json:"title,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	TouchedAt string `json:"touched_at,omitempty"`
	// ServedNow marks a PR-existence discrepancy whose repo has a LIVE
	// pulls-list marker: the cached (wrong) list is being served right now.
	// Without a marker the next list read misses, re-fetches, and self-heals.
	ServedNow bool   `json:"served_now,omitempty"`
	Note      string `json:"note,omitempty"`
	// Fix is a short remediation hint for this discrepancy class.
	Fix string `json:"fix,omitempty"`
}

// Check runs the read-only consistency check for the global truth store. When
// orgFilter is non-empty only that owner is checked; otherwise every owner
// with cached repos is checked. The cache is never modified.
func (c *ConsistencyChecker) Check(ctx context.Context, orgFilter string) (*ConsistencyReport, error) {
	return c.run(ctx, orgFilter, false, nil)
}

// CheckAndApply runs the consistency check and then CORRECTS the drift from
// the same fetched snapshot (see applyOwner). The report's discrepancies show
// the PRE-apply state; Applied tallies the corrections written.
func (c *ConsistencyChecker) CheckAndApply(ctx context.Context, orgFilter string) (*ConsistencyReport, error) {
	return c.run(ctx, orgFilter, true, nil)
}

func (c *ConsistencyChecker) run(ctx context.Context, orgFilter string, apply bool, progress ProgressFunc) (*ConsistencyReport, error) {
	if c.app == nil {
		return nil, fmt.Errorf("consistency check unavailable: no GitHub App configured (set GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY)")
	}

	report := &ConsistencyReport{
		FetchedAs:     "github-app",
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		OrgsChecked:   []string{},
		Discrepancies: []Discrepancy{},
		Notes: []string{
			"Source of truth was fetched as the mirror's GitHub App (repositoryOwner query, so User-account installations are checked too). Owners the app is not installed on are skipped (listed under orgs_skipped), not reported as missing.",
			"Only OPEN pull requests are compared (the cache only retains open PRs). A PR shown as only_in_cache is cached as open but is not in GitHub's current open set, i.e. it was likely closed/merged and a webhook was missed.",
			"A repo reported only_on_github with a private/internal visibility has simply never been absorbed (no webhook and no principal's sync has touched it) -- truth is lazy, so this is expected until something references the repo. Such repos are tallied separately in repos_only_on_github_private.",
			"The mergeable field is not compared: the cache deliberately un-resolves it on pushes and the GraphQL/REST readings race GitHub's recomputation.",
			"pushed_at is compared with a " + pushedAtTolerance.String() + " tolerance: only a cached value lagging GitHub by more than that is drift (it implies missed push webhooks, and therefore possibly stale contents_cache rows).",
		},
	}
	if apply {
		report.Applied = &AppliedSummary{}
		report.Notes = append(report.Notes,
			"Apply mode: corrections were written AFTER the diff was taken -- discrepancies show the PRE-apply state, and 'applied' tallies the corrections.")
	}

	// Load the global truth once.
	repos, err := c.store.AllRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("load cached repos: %w", err)
	}
	openPRs, err := c.store.AllOpenPullRequests(ctx)
	if err != nil {
		return nil, fmt.Errorf("load cached open PRs: %w", err)
	}
	labels, err := c.store.AllPRLabels(ctx)
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
	progress.emit(ProgressEvent{Phase: "start", Owners: len(owners)})

	// Resolve App installations once so we know which owners are reachable.
	installs, err := c.app.Installations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list GitHub App installations: %w", err)
	}
	byLogin := make(map[string]ghclient.Installation, len(installs))
	for _, in := range installs {
		byLogin[strings.ToLower(in.Account.Login)] = in
	}

	// Principal display names for the freshness markers, loaded once per run.
	actorNames := c.actorNames(ctx)

	for i, owner := range owners {
		progress.emit(ProgressEvent{Phase: "owner", Owner: owner, Index: i + 1, Total: len(owners)})

		// The owner's most-recent sync staleness, whether or not the owner
		// ends up checked -- long-unsynced truth explains drift. (In apply
		// mode this reads the PRE-apply marker; the apply stamps a fresh one.)
		c.recordTruthFreshness(ctx, report, owner, actorNames)

		skip := func(reason string) {
			report.OrgsSkipped = append(report.OrgsSkipped, OrgSkip{Org: owner, Reason: reason})
			progress.emit(ProgressEvent{Phase: "skip", Owner: owner, Reason: reason})
		}

		inst, ok := byLogin[strings.ToLower(owner)]
		if !ok {
			skip("no GitHub App installation for this owner (app not installed, or no access)")
			continue
		}

		token, err := c.app.InstallationToken(ctx, inst.ID)
		if err != nil {
			skip("could not mint installation token: " + err.Error())
			continue
		}
		fetchCtx := ghclient.WithToken(ctx, token)
		fetchStart := time.Now()
		// Per fetched page (5 repos each), report how far along the owner's
		// repo fetch is -- the dominant cost of a large owner's check.
		var onPage ghclient.OwnerPageFunc
		if progress != nil {
			onPage = func(fetched, total int) {
				progress.emit(ProgressEvent{Phase: "fetch", Owner: owner, ReposFetched: fetched, ReposTotal: total})
			}
		}
		data, err := c.gh.GetOwnerDataWithProgress(fetchCtx, owner, onPage)
		if err != nil {
			skip("fetch from GitHub failed: " + err.Error())
			continue
		}

		// Repo visibility + archive state as the App sees it, via the
		// checker-private query (NEVER the shared cached-route query).
		// Best-effort: without it the diff still runs, but visibility diffs
		// and missing-repo private/archived classification are unavailable --
		// which the report says out loud instead of silently reading as clean.
		progress.emit(ProgressEvent{Phase: "visibility", Owner: owner})
		visibility, verr := c.gh.OwnerRepoVisibilities(fetchCtx, owner)
		if verr != nil {
			slog.Warn("consistency: fetch repo visibility failed", "owner", owner, "error", verr)
			report.Notes = append(report.Notes, fmt.Sprintf(
				"owner %s: repo-visibility fetch failed (%v) -- visibility diffs and private/archived classification are unavailable for this owner in this report", owner, verr))
			visibility = nil
		}

		report.OrgsChecked = append(report.OrgsChecked, owner)
		c.diffOwner(ctx, report, owner, reposByOwner[owner], prsByOwnerRepo, labelsByRepoPR, data, visibility)
		progress.emit(ProgressEvent{Phase: "diffed", Owner: owner, Discrepancies: len(report.Discrepancies)})

		if apply {
			if err := c.applyOwner(ctx, report.Applied, owner, inst, reposByOwner[owner], prsByOwnerRepo, data, visibility, fetchStart); err != nil {
				slog.Warn("consistency: apply failed", "owner", owner, "error", err)
				report.Notes = append(report.Notes, fmt.Sprintf("owner %s: apply failed partway: %v", owner, err))
			}
			// Snapshot the tally: the report's pointer keeps mutating as later
			// owners apply, so the event must carry its own copy.
			applied := *report.Applied
			progress.emit(ProgressEvent{Phase: "applied", Owner: owner, Applied: &applied})
		}
	}

	// Attach the per-class remediation hints.
	for i := range report.Discrepancies {
		report.Discrepancies[i].Fix = fixHint(report.Discrepancies[i])
	}

	// Finalize summary counts.
	report.Summary.OrgsChecked = len(report.OrgsChecked)
	for _, d := range report.Discrepancies {
		switch d.Issue {
		case "only_in_cache":
			if d.Kind == "repo" {
				report.Summary.ReposOnlyInCache++
				if d.Archived {
					report.Summary.ReposOnlyInCacheArchived++
				}
			} else {
				report.Summary.PRsOnlyInCache++
			}
		case "only_on_github":
			if d.Kind == "repo" {
				report.Summary.ReposOnlyOnGitHub++
				if d.Visibility == ghdata.VisibilityPrivate || d.Visibility == "internal" {
					report.Summary.ReposOnlyOnGitHubPrivate++
				}
			} else {
				report.Summary.PRsOnlyOnGitHub++
			}
		case "field_mismatch":
			report.Summary.FieldMismatches++
		case "visibility_leak":
			report.Summary.VisibilityLeaks++
			report.Summary.FieldMismatches++
		}
	}
	report.Summary.Discrepancies = len(report.Discrepancies)
	progress.emit(ProgressEvent{Phase: "done"})
	return report, nil
}

// actorNames returns the recorded principal->display-name map (from
// actor_identities), or an empty map when the lookup is unavailable or fails
// — the report's principal keys then simply carry no name.
func (c *ConsistencyChecker) actorNames(ctx context.Context) map[string]string {
	names := make(map[string]string)
	if c.store == nil {
		return names
	}
	identities, err := c.store.ListActorIdentities(ctx)
	if err != nil {
		return names
	}
	for _, id := range identities {
		names[id.Actor] = id.Login
	}
	return names
}

// recordTruthFreshness copies the most recently fetched org-sync marker for
// one owner into the report (read-only; no markers adds nothing). Any
// principal's sync refreshes global truth, so the NEWEST marker is what
// bounds truth staleness. names resolves the marker's principal key to its
// recorded display name (best-effort).
func (c *ConsistencyChecker) recordTruthFreshness(ctx context.Context, report *ConsistencyReport, owner string, names map[string]string) {
	if c.fresh == nil {
		return
	}
	metas, err := c.fresh.ListByKindKeyAllActors(ctx, KindOrgRepos, owner)
	if err != nil || len(metas) == 0 {
		return
	}
	var newest *freshness.Metadata
	for i := range metas {
		m := &metas[i]
		if m.LastFetchedAt == nil {
			continue
		}
		if newest == nil || newest.LastFetchedAt == nil || m.LastFetchedAt.After(*newest.LastFetchedAt) {
			newest = m
		}
	}
	if newest == nil {
		newest = &metas[0]
	}
	sf := ScopeFreshness{State: string(newest.State), Error: newest.ErrorMessage, Principal: newest.Actor, PrincipalName: names[newest.Actor]}
	if newest.LastFetchedAt != nil {
		sf.LastFetchedAt = newest.LastFetchedAt.UTC().Format(time.RFC3339)
	}
	if report.TruthFreshness == nil {
		report.TruthFreshness = make(map[string]ScopeFreshness)
	}
	report.TruthFreshness[owner] = sf
}
