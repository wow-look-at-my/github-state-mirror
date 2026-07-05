package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The consistency check's APPLY half (POST /api/cache/check?apply=true): the
// corrections written from the already-fetched snapshot, plus the per-class
// remediation hints the report attaches.

// applyOwner corrects the drift for one owner from the already-fetched
// snapshot, under the installation's stable principal. The diff has already
// run against the pre-apply cache, so the report reads as before/after.
func (c *ConsistencyChecker) applyOwner(
	ctx context.Context,
	ap *AppliedSummary,
	owner string,
	inst ghclient.Installation,
	cachedRepos map[string]dbgen.Repo,
	cachedPRs map[string]map[int64]dbgen.PullRequest,
	data *ghclient.OrgData,
	visibility map[string]ghclient.OwnerRepoVisibility,
	fetchStart time.Time,
) error {
	now := time.Now()
	principal := AppInstallationActor(inst.ID)

	// 1. Absorb the snapshot: repos + open PRs + labels upserted, stale
	// cached-open PRs deleted (grace-guarded reconcile), the principal's
	// grants replace-synced. Tally intent against the pre-apply cache -- the
	// same view the diff used.
	sync := ghdata.OrgSyncData{Repos: data.Repos, PRsByRepo: data.PRsByRepo, LabelsByPR: data.LabelsByPR}
	if err := c.store.SyncOrgTruth(ctx, owner, sync, principal, fetchStart, now); err != nil {
		return fmt.Errorf("sync truth: %w", err)
	}
	fetchedNames := make(map[string]bool, len(data.Repos))
	for _, r := range data.Repos {
		fetchedNames[r.Name] = true
		repoKey := owner + "/" + r.Name
		if _, ok := cachedRepos[r.Name]; !ok {
			ap.ReposAbsorbed++
		}
		fetched := make(map[int64]bool, len(data.PRsByRepo[r.NameWithOwner]))
		for _, pr := range data.PRsByRepo[r.NameWithOwner] {
			fetched[pr.Number] = true
			if _, ok := cachedPRs[repoKey][pr.Number]; !ok {
				ap.PRsAbsorbed++
			}
		}
		for num := range cachedPRs[repoKey] {
			if !fetched[num] {
				ap.PRsDeleted++
			}
		}
	}

	// 2. Visibility, from the checker-private map: fixes fail-closed '' rows
	// AND drift in both directions (a leak -- cached public, GitHub private --
	// and the reverse). Only rows that exist are written (a repo neither
	// cached nor fetched, e.g. archived and never absorbed, has no row).
	for name, vis := range visibility {
		if vis.Visibility == "" {
			continue
		}
		cr, wasCached := cachedRepos[name]
		if !wasCached && !fetchedNames[name] {
			continue
		}
		if wasCached && cr.Visibility == vis.Visibility {
			continue
		}
		if err := c.store.SetRepoVisibility(ctx, owner, name, vis.Visibility); err != nil {
			return fmt.Errorf("set visibility %s/%s: %w", owner, name, err)
		}
		ap.VisibilitySet++
	}

	for _, r := range data.Repos {
		// 3. default_branch_status: explicit set INCLUDING NULL where
		// mismatched -- the COALESCE upsert can never null a stale rollup
		// (the gcc/.github drift class: the tip advanced to a commit with no
		// CI and the old rollup stuck forever).
		if cr, ok := cachedRepos[r.Name]; ok && ns(cr.DefaultBranchStatus) != ns(r.DefaultBranchStatus) {
			if err := c.store.SetRepoDefaultBranchStatus(ctx, owner, r.Name, r.DefaultBranchStatus); err != nil {
				return fmt.Errorf("set default branch status %s/%s: %w", owner, r.Name, err)
			}
			ap.DefaultBranchStatusSet++
		}

		repoKey := owner + "/" + r.Name
		for _, fpr := range data.PRsByRepo[r.NameWithOwner] {
			// 4. The commit_checks stick rule. GitHub's rollup for the PR's
			// FRESH head sha is the verdict; the webhook-aggregated rows are
			// corrected by deletion, never by synthesizing per-check rows (a
			// fabricated completed row would be tomorrow's ghost).
			sha := ns(fpr.HeadRefOid)
			ghRollup := ns(fpr.LastCommitStatus)
			if sha != "" {
				switch ghRollup {
				case "SUCCESS", "FAILURE", "ERROR":
					states, err := c.store.CommitCheckStates(ctx, owner, r.Name, sha)
					if err != nil {
						return err
					}
					// Only ROWS that contradict the terminal verdict need the
					// correction: with zero rows there is nothing to poison the
					// next recompute, and the sync's COALESCE upsert already
					// stamped the non-null verdict on the column -- correcting
					// anyway would tally every checks-less PR as "corrected" on
					// every apply, forever.
					if len(states) > 0 && ghdata.RollupState(states) != ghRollup {
						if err := c.store.ForceCheckRollup(ctx, owner, r.Name, sha, sql.NullString{String: ghRollup, Valid: true}); err != nil {
							return fmt.Errorf("force rollup %s#%d: %w", repoKey, fpr.Number, err)
						}
						ap.StatusesCorrected++
						ap.CheckRowsDeleted += len(states)
					}
				case "":
					// GitHub reports NO rollup (e.g. the head advanced to a
					// commit with no checks). Leftover rows or a non-NULL
					// cached column are stale state from a previous sha.
					states, err := c.store.CommitCheckStates(ctx, owner, r.Name, sha)
					if err != nil {
						return err
					}
					cachedCol := ""
					if cpr, ok := cachedPRs[repoKey][fpr.Number]; ok {
						cachedCol = ns(cpr.LastCommitStatus)
					}
					if len(states) > 0 || cachedCol != "" {
						if err := c.store.ForceCheckRollup(ctx, owner, r.Name, sha, sql.NullString{}); err != nil {
							return fmt.Errorf("clear rollup %s#%d: %w", repoKey, fpr.Number, err)
						}
						ap.StatusesCorrected++
						ap.CheckRowsDeleted += len(states)
					}
				default:
					// PENDING/EXPECTED: not a terminal verdict -- completions
					// will recompute; touching rows here only loses real state.
				}
			}

			// 5. auto_merge_method: the upsert never applies it from
			// GraphQL-shaped rows, so a mismatch needs the explicit set
			// (including NULL -- a stale armed flag silently disables
			// pr-minder's merge backstop).
			cachedArm := ""
			if cpr, ok := cachedPRs[repoKey][fpr.Number]; ok {
				cachedArm = ns(cpr.AutoMergeMethod)
			}
			if cachedArm != ns(fpr.AutoMergeMethod) {
				if err := c.store.SetPRAutoMergeMethod(ctx, owner, r.Name, fpr.Number, fpr.AutoMergeMethod); err != nil {
					return fmt.Errorf("set auto merge %s#%d: %w", repoKey, fpr.Number, err)
				}
				ap.AutoMergeSet++
			}
		}
	}

	// 6. Stamp the app-installation freshness marker so truth_freshness (and
	// the dashboard) reflect that this owner's truth was just refreshed.
	// Best-effort: the corrections above already landed.
	if c.fresh != nil {
		exp := now.Add(6 * time.Hour) // the org_repos policy TTL
		lastFetched := now
		if err := c.fresh.Upsert(ctx, &freshness.Metadata{
			ResourceID:    freshness.ResourceID{Kind: KindOrgRepos, Key: owner, Actor: principal},
			State:         freshness.StateFresh,
			LastFetchedAt: &lastFetched,
			ExpiresAt:     &exp,
		}); err != nil {
			slog.Warn("consistency: stamp freshness marker failed", "owner", owner, "error", err)
		}
	}
	return nil
}

// fixHint returns the short remediation hint for a discrepancy class.
func fixHint(d Discrepancy) string {
	switch d.Issue {
	case "only_in_cache":
		if d.Kind == "repo" {
			if d.Archived {
				return "none needed: archived repos stay cached by design"
			}
			return "verify on GitHub; repo deletion authority is the repository webhook (apply mode never deletes repos)"
		}
		return "apply mode deletes the stale open row; a mirrored read of the PR also self-heals it"
	case "only_on_github":
		if d.Kind == "repo" {
			return "apply mode absorbs it (POST /api/cache/check?apply=true)"
		}
		return "apply mode absorbs it; any webhook or mirrored list read for the repo also does"
	case "visibility_leak", "visibility_unknown":
		return "apply mode sets visibility from GitHub's answer"
	case "field_mismatch":
		switch d.Field {
		case "last_commit_status":
			return "apply mode deletes the contradicted commit_checks rows and sets GitHub's rollup, so the next webhook cannot re-poison it"
		case "default_branch_status":
			return "apply mode sets GitHub's value (including null)"
		case "auto_merge_method":
			return "apply mode sets GitHub's armed state (including null)"
		case "pushed_at":
			return "apply mode overwrites it via the truth sync; audit the repo's webhook deliveries -- its contents_cache may have served stale files"
		default:
			return "apply mode overwrites it via the truth sync"
		}
	}
	return ""
}
