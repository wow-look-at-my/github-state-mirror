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

// The consistency check's DIFF half: pure comparisons of cached truth against
// the freshly fetched owner snapshot. Nothing here writes to the store (the
// pulls-list marker probe is a pure read); the corrections live in
// consistency_apply.go.

// diffOwner compares the cached repos/PRs/labels for one owner against the data
// freshly fetched from GitHub, appending discrepancies to the report.
// visibility (repo name -> live visibility + archive state, as the App sees it,
// INCLUDING archived repos) classifies missing repos and feeds the visibility/
// is_archived diffs; nil means it could not be fetched and no classification is
// added. checkStart is when the whole run began (captured before any fetch):
// a GitHub-side timestamp at or after it proves the resource moved WHILE the
// check ran, which is classified raced_during_check instead of drift.
func (c *ConsistencyChecker) diffOwner(
	ctx context.Context,
	report *ConsistencyReport,
	owner string,
	cachedRepos map[string]dbgen.Repo,
	cachedPRs map[string]map[int64]dbgen.PullRequest,
	cachedLabels map[string]map[int64]map[string]string,
	data *ghclient.OrgData,
	visibility map[string]ghclient.OwnerRepoVisibility,
	checkStart time.Time,
) {
	now := time.Now()
	// servedNow memoizes the repo's live pulls-list marker: a PR-existence
	// discrepancy under a live marker is being SERVED wrong right now (the
	// list route trusts the rows); without one it self-heals on the next read.
	markerByRepo := map[string]bool{}
	servedNow := func(name string) bool {
		if v, ok := markerByRepo[name]; ok {
			return v
		}
		live, err := c.store.HasLivePullsListMarker(ctx, owner, name, now)
		if err != nil {
			live = false
		}
		markerByRepo[name] = live
		return live
	}

	// --- repos ---
	freshRepos := make(map[string]dbgen.Repo, len(data.Repos))
	for _, r := range data.Repos {
		freshRepos[r.Name] = r
	}
	for name, cr := range cachedRepos {
		fr, ok := freshRepos[name]
		if !ok {
			d := Discrepancy{
				Kind: "repo", Repo: owner + "/" + name, Issue: "only_in_cache",
				Cached: cr.Url,
				Note:   "cached but not among GitHub's non-archived repos (archived, deleted, renamed, or no longer visible)",
			}
			vis, known := visibility[name]
			switch {
			case known && vis.Archived:
				d.Archived = true
				d.Note = "archived on GitHub; archived repos are excluded from the org data fetch -- expected, not drift"
				if cr.IsArchived == 0 {
					report.Discrepancies = append(report.Discrepancies, Discrepancy{
						Kind: "repo", Repo: owner + "/" + name, Issue: "field_mismatch", Field: "is_archived",
						Cached: "false", GitHub: "true",
						Note: "GitHub reports the repo archived but the cached row reads active (missed repository.archived webhook)",
					})
				}
			case cr.IsArchived != 0:
				d.Archived = true
				d.Note = "cached row is marked archived; archived repos are excluded from the org data fetch -- expected, not drift"
			case known:
				d.Note = "visible to the App and not archived, yet absent from its org data fetch (renamed, or a data/visibility race)"
			}
			report.Discrepancies = append(report.Discrepancies, d)
			continue
		}
		report.Discrepancies = append(report.Discrepancies, repoFieldDiffs(owner, name, cr, fr, visibility, checkStart)...)
	}
	for name, fr := range freshRepos {
		if _, ok := cachedRepos[name]; !ok {
			d := Discrepancy{
				Kind: "repo", Repo: owner + "/" + name, Issue: "only_on_github",
				GitHub: fr.Url,
				Note:   "exists on GitHub but has not been absorbed into global truth",
			}
			if vis, known := visibility[name]; known && vis.Visibility != "" && vis.Visibility != ghdata.VisibilityPublic {
				d.Visibility = vis.Visibility
				d.Note = vis.Visibility + " repo not yet absorbed: no webhook and no principal's sync has referenced it; expected under lazy truth"
			}
			report.Discrepancies = append(report.Discrepancies, d)
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
					Title:  cpr.Title, UpdatedAt: cpr.UpdatedAt, TouchedAt: cpr.TouchedAt,
					ServedNow: servedNow(repoName),
					Note:      "cached as open but not in GitHub's open PRs (likely closed/merged; a webhook was missed)",
				})
				continue
			}
			report.Discrepancies = append(report.Discrepancies, prFieldDiffs(repoKey, num, cpr, fpr, checkStart)...)
			report.Discrepancies = append(report.Discrepancies, labelDiffs(repoKey, num,
				cachedLabels[repoKey][num], freshLabelSet(data.LabelsByPR[fr.NameWithOwner][num]))...)
		}
		for num, fpr := range freshPRs {
			if _, ok := cached[num]; !ok {
				report.Discrepancies = append(report.Discrepancies, Discrepancy{
					Kind: "pr", Repo: repoKey, PR: num, Issue: "only_on_github",
					GitHub:    fpr.Url,
					ServedNow: servedNow(repoName),
					Note:      "open on GitHub but not in global truth",
				})
			}
		}
	}

	// Cached-open PRs under repos ABSENT from GitHub's fetched set are never
	// visited above -- sweep them so stale-but-servable rows can't hide behind
	// the repo-level only_in_cache entry.
	for repoKey, prs := range cachedPRs {
		if !strings.HasPrefix(repoKey, owner+"/") {
			continue
		}
		name := strings.TrimPrefix(repoKey, owner+"/")
		if _, ok := freshRepos[name]; ok {
			continue
		}
		for num, cpr := range prs {
			report.Discrepancies = append(report.Discrepancies, Discrepancy{
				Kind: "pr", Repo: repoKey, PR: num, Issue: "only_in_cache",
				Cached: cpr.Url,
				Title:  cpr.Title, UpdatedAt: cpr.UpdatedAt, TouchedAt: cpr.TouchedAt,
				ServedNow: servedNow(name),
				Note:      "cached open under a repo not among GitHub's non-archived repos (see the repo's own entry)",
			})
		}
	}
}

// pushedAtTolerance absorbs the race between the check's fetch and live
// pushes: only a cached pushed_at lagging GitHub's by MORE than this is
// reported. The signal being hunted is missed push webhooks (which also mean
// un-flushed contents_cache rows), not seconds of skew.
const pushedAtTolerance = 5 * time.Minute

// pushedAtDrift reports whether the cached pushed_at lags GitHub's by more
// than the tolerance (and by how much). A cached NULL with a GitHub value is
// drift (no push was ever recorded); unparseable/absent GitHub values are not.
func pushedAtDrift(cached, github sql.NullString) (time.Duration, bool) {
	if !github.Valid || github.String == "" {
		return 0, false
	}
	gt, err := time.Parse(time.RFC3339, github.String)
	if err != nil {
		return 0, false
	}
	if !cached.Valid || cached.String == "" {
		return 0, true
	}
	ct, err := time.Parse(time.RFC3339, cached.String)
	if err != nil {
		return 0, true
	}
	d := gt.Sub(ct)
	return d, d > pushedAtTolerance
}

// racedDuringCheck reports whether a GitHub-side RFC3339 timestamp is at or
// after the check's start: the resource provably moved WHILE the check ran, so
// the cached value never had a chance to catch up and the difference is race,
// not drift. The boundary is >= (a value exactly at check start already
// postdates the cached rows the diff read). A zero checkStart or an
// unparseable/absent timestamp never claims a race -- fail toward reporting
// drift.
func racedDuringCheck(github string, checkStart time.Time) bool {
	if checkStart.IsZero() || github == "" {
		return false
	}
	gt, err := time.Parse(time.RFC3339, github)
	if err != nil {
		return false
	}
	return !gt.Before(checkStart)
}

// noneMarker renders an absent value explicitly. The Discrepancy JSON fields
// are omitempty (so only_* entries stay clean), which silently DROPPED the
// github side of a default_branch mismatch whenever GraphQL reported no
// defaultBranchRef (an empty repo has no ref even though REST reports a
// configured default_branch name) -- the 2026-07-20 report's value-less
// entries. An explicit marker keeps both sides always present for fields
// where emptiness is a real answer.
const noneMarker = "(none)"

func orNone(v sql.NullString) string {
	if s := ns(v); s != "" {
		return s
	}
	return noneMarker
}

// repoFieldDiffs compares the webhook-fed / refreshed repo fields, including
// visibility (the reveal layer's security-load-bearing field), is_archived,
// and tolerance-guarded pushed_at. checkStart gates the raced_during_check
// classification (see racedDuringCheck).
func repoFieldDiffs(owner, name string, c, g dbgen.Repo, visibility map[string]ghclient.OwnerRepoVisibility, checkStart time.Time) []Discrepancy {
	repoKey := owner + "/" + name
	var out []Discrepancy
	add := func(field, cv, gv string) {
		if cv != gv {
			out = append(out, Discrepancy{Kind: "repo", Repo: repoKey, Issue: "field_mismatch", Field: field, Cached: cv, GitHub: gv})
		}
	}
	add("default_branch_status", ns(c.DefaultBranchStatus), ns(g.DefaultBranchStatus))

	// default_branch gets its own constructor so the entry ALWAYS carries an
	// explicit github value: GraphQL's defaultBranchRef is null for a repo with
	// no commits (REST still reports the CONFIGURED default_branch name, which
	// is what the webhook/REST absorb paths cache), and the bare add() rendered
	// that as "" -- dropped from the JSON by omitempty, leaving a one-sided
	// entry.
	if ns(c.DefaultBranch) != ns(g.DefaultBranch) {
		d := Discrepancy{
			Kind: "repo", Repo: repoKey, Issue: "field_mismatch", Field: "default_branch",
			Cached: orNone(c.DefaultBranch), GitHub: orNone(g.DefaultBranch),
		}
		if !g.DefaultBranch.Valid || g.DefaultBranch.String == "" {
			d.Note = "GitHub's fetch reported no default branch ref (GraphQL defaultBranchRef is null -- typically a repo with no commits); the cached name is REST's configured default_branch"
		}
		out = append(out, d)
	}

	add("is_disabled", boolStr(c.IsDisabled), boolStr(g.IsDisabled))
	add("url", c.Url, g.Url)

	vis, visKnown := visibility[name]

	// is_archived: a repo in the org data fetch is non-archived by the query's
	// own filter; prefer the visibility map's authoritative answer when known.
	ghArchived := g.IsArchived != 0
	if visKnown {
		ghArchived = vis.Archived
	}
	if (c.IsArchived != 0) != ghArchived {
		add("is_archived", boolStr(c.IsArchived), fmt.Sprintf("%t", ghArchived))
	}

	// pushed_at, tolerance-guarded: lag means missed push webhooks, which also
	// means the repo's contents_cache was never flushed for those pushes. A
	// GitHub-side pushed_at at or after the check's start is a push that landed
	// WHILE the check ran -- the cache could not possibly have it yet, so it is
	// classified raced_during_check (informational), not drift.
	if lag, drifted := pushedAtDrift(c.PushedAt, g.PushedAt); drifted {
		if racedDuringCheck(ns(g.PushedAt), checkStart) {
			out = append(out, Discrepancy{
				Kind: "repo", Repo: repoKey, Issue: "raced_during_check", Field: "pushed_at",
				Cached: ns(c.PushedAt), GitHub: ns(g.PushedAt),
				Note: "GitHub's pushed_at postdates the check's start: the repo was pushed while the check ran -- informational, not drift",
			})
		} else {
			note := "cached pushed_at lags GitHub: push webhook(s) were likely missed, so this repo's contents_cache may have served stale files"
			if lag > 0 {
				note += fmt.Sprintf(" (lag %s)", lag.Round(time.Second))
			}
			out = append(out, Discrepancy{
				Kind: "repo", Repo: repoKey, Issue: "field_mismatch", Field: "pushed_at",
				Cached: ns(c.PushedAt), GitHub: ns(g.PushedAt), Note: note,
			})
		}
	}

	// visibility: cached-public/GitHub-nonpublic is the dangerous direction
	// (the reveal fast path serves public rows to ANY authenticated caller),
	// so it gets its own issue string. A cached '' is fail-closed private --
	// informational, not drift.
	if visKnown && vis.Visibility != "" && c.Visibility != vis.Visibility {
		switch {
		case c.Visibility == ghdata.VisibilityUnknown:
			out = append(out, Discrepancy{
				Kind: "repo", Repo: repoKey, Issue: "visibility_unknown", Field: "visibility",
				Cached: "", GitHub: vis.Visibility,
				Note: "cached visibility is unknown (treated as private, fail closed) -- informational, not drift",
			})
		case c.Visibility == ghdata.VisibilityPublic:
			out = append(out, Discrepancy{
				Kind: "repo", Repo: repoKey, Issue: "visibility_leak", Field: "visibility",
				Cached: c.Visibility, GitHub: vis.Visibility,
				Note: "SECURITY: cached public but " + vis.Visibility + " on GitHub -- the reveal fast path is serving this repo's cached state to any authenticated caller",
			})
		default:
			out = append(out, Discrepancy{
				Kind: "repo", Repo: repoKey, Issue: "field_mismatch", Field: "visibility",
				Cached: c.Visibility, GitHub: vis.Visibility,
			})
		}
	}
	return out
}

// prFieldDiffs compares the webhook-fed / refreshed PR fields. created_at and
// updated_at are intentionally not compared (updated_at churns constantly and is
// not a correctness signal); mergeable is skipped (see the report notes).
// checkStart gates head_ref_oid's raced_during_check classification: the
// in-flight-movement proof is the PR's own GitHub-side updated_at (a head push
// bumps it, and it is the timestamp the owner-query snapshot actually carries
// -- the repo's pushed_at is not threaded down here).
func prFieldDiffs(repoKey string, num int64, c, g dbgen.PullRequest, checkStart time.Time) []Discrepancy {
	var out []Discrepancy
	add := func(field, cv, gv string) {
		if cv != gv {
			out = append(out, Discrepancy{Kind: "pr", Repo: repoKey, PR: num, Issue: "field_mismatch", Field: field, Cached: cv, GitHub: gv})
		}
	}
	add("title", c.Title, g.Title)
	add("is_draft", boolStr(c.IsDraft), boolStr(g.IsDraft))
	add("last_commit_status", ns(c.LastCommitStatus), ns(g.LastCommitStatus))

	// head_ref_oid: a differing head sha whose PR was updated at/after the
	// check's start moved WHILE the check ran (the push's webhook simply had no
	// time to land) -- raced_during_check, not drift.
	if ns(c.HeadRefOid) != ns(g.HeadRefOid) {
		if racedDuringCheck(g.UpdatedAt, checkStart) {
			out = append(out, Discrepancy{
				Kind: "pr", Repo: repoKey, PR: num, Issue: "raced_during_check", Field: "head_ref_oid",
				Cached: ns(c.HeadRefOid), GitHub: ns(g.HeadRefOid),
				Note: "the PR's GitHub-side updated_at postdates the check's start: its head moved while the check ran -- informational, not drift",
			})
		} else {
			add("head_ref_oid", ns(c.HeadRefOid), ns(g.HeadRefOid))
		}
	}

	add("head_ref_name", ns(c.HeadRefName), ns(g.HeadRefName))
	add("base_ref_name", ns(c.BaseRefName), ns(g.BaseRefName))
	add("review_request_count", ni(c.ReviewRequestCount), ni(g.ReviewRequestCount))
	add("auto_merge_method", ns(c.AutoMergeMethod), ns(g.AutoMergeMethod))
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
