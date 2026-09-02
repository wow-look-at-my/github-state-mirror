package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The apply half of the response-cache pass. Its sibling,
// webhook_invalidate.go, decides WHICH rows a delivery reaches; what lives
// here is every case where the payload states the new answer, so the row is
// rewritten instead of dropped (CLAUDE.md: apply the payload, invalidation is
// the last resort). docs/webhooks/invalidations.md is the inventory, with the
// verdict for every flush that remains.

// settleCommitCI lands a CI delivery on the cached commit-CI documents, at the grain the delivery can move.
// see docs/webhooks/invalidations.md ("The status rewrite", "The check-run rewrite")
func (d *WebhookDispatcher) settleCommitCI(ctx context.Context, scope, owner, repo string, event webhook.Event, refs []string) {
	if event.Type == "check_run" {
		// A check run is UPDATED in place upstream -- the same id moves
		// queued -> in_progress -> completed -- so the delivery states the new
		// value of entry and the page keeps its shape.
		if raw, ok := webhook.CheckRunObject(event.Raw); ok {
			if run, ok := ghdata.TrimCheckRunJSON(raw); ok {
				// The single-check-run row (respcache_checkrun.go) is keyed by
				// this run's own id, so the delivery is ALWAYS directly
				if err := d.store.ApplyCheckRunByID(ctx, owner, repo, run, time.Now(), ghdata.CheckRunCacheTTL); err != nil {
					flush("check run by-id apply", scope, err)
				}
				applied, err := d.store.ApplyCheckRunToCommitCI(ctx, owner, repo, run, time.Now(), ghdata.CommitCICacheTTL)
				if err != nil {
					flush("check run apply", scope, err)
				}
				if applied {
					return
				}
			}
		}
	}
	if event.Type != "status" {
		for _, ref := range refs {
			flush("commit CI cache", scope, d.store.InvalidateCommitCIForRefKind(ctx, owner, repo, ref, ghdata.CommitCIKindCheckRuns))
		}
		return
	}
	up, ok := webhook.ParseStatusEvent(event.Raw)
	if !ok {
		// A status payload that does not state its own status: no rewrite is
		// possible, so keep the flush for the kinds it could have moved.
		for _, ref := range refs {
			flush("commit CI cache", scope, d.store.InvalidateCommitCIForRefKind(ctx, owner, repo, ref, ghdata.CommitCIKindStatus))
			flush("commit CI cache", scope, d.store.InvalidateCommitCIForRefKind(ctx, owner, repo, ref, ghdata.CommitCIKindStatusesList))
		}
		return
	}
	flush("commit CI settle", scope, d.store.SettleCommitCIFromStatus(ctx, owner, repo, refs, ghdata.CommitStatusUpdate{
		SHA: up.SHA, Context: up.Context, State: up.State,
		Description: up.Description, TargetURL: up.TargetURL,
		CreatedAt: up.CreatedAt, UpdatedAt: up.UpdatedAt,
	}, time.Now(), ghdata.CommitCICacheTTL))
}

// applyMergedPRBaseTip writes a merged PR's merge_commit_sha onto its base branch's cached ref rows,
// deliberately the way to hear a merge tip (the base push is the). see docs/cache/stale-tip-repair.md
func (d *WebhookDispatcher) applyMergedPRBaseTip(ctx context.Context, scope, owner, repo string, event webhook.Event) {
	tip, ok := webhook.ParseMergedPRBaseTip(event.Raw)
	if !ok {
		return // not a merged PR, or the payload states no base tip
	}
	now := time.Now()
	for _, ref := range refSpellings(tip.BaseRef, false) {
		applied, err := d.store.ApplyMergedBaseTip(ctx, owner, repo, ref, tip.MergeCommitSHA, tip.MergedAt, now, ghdata.GitRefCacheTTL)
		if err != nil {
			flush("merged-PR base tip apply", scope, err)
			continue
		}
		if applied {
			slog.Info("webhook: applied merged PR's base tip to the cached ref",
				"repo", scope, "ref", ref, "tip", tip.MergeCommitSHA, "merged_at", tip.MergedAt)
		}
	}
}

// settleWorkflowJobs rewrites the run's cached job entry from the delivery, flushing only what it cannot answer.
// see docs/webhooks/invalidations.md ("Live job state")
func (d *WebhookDispatcher) settleWorkflowJobs(ctx context.Context, scope, owner, repo string, event webhook.Event, runID int64) {
	if runID > 0 {
		if raw, ok := webhook.WorkflowJobObject(event.Raw); ok {
			if job, ok := ghdata.TrimWorkflowJobJSON(raw); ok {
				applied, err := d.store.ApplyWorkflowJob(ctx, owner, repo, runID, job, time.Now())
				if err != nil {
					flush("workflow job apply", scope, err)
				}
				if applied {
					return
				}
			}
		}
	}
	d.flushWorkflowJobsForRun(ctx, scope, owner, repo, runID)
}

// settleWorkflowRuns lands a `workflow_run` delivery on the sha's cached runs
// pages. The delivery IS the run object those pages list, so its entry is
// rewritten where it stands; a run the pages do not list is a new run for the
// sha, a membership change only a fetch settles, and falls back to the flush.
func (d *WebhookDispatcher) settleWorkflowRuns(ctx context.Context, scope, owner, repo string, event webhook.Event, headSHA string) {
	if raw, ok := webhook.WorkflowRunObject(event.Raw); ok {
		if run, ok := ghdata.TrimWorkflowRunItemJSON(raw); ok {
			applied, err := d.store.ApplyWorkflowRunToPages(ctx, owner, repo, run, time.Now(), ghdata.WorkflowRunsCacheTTL)
			if err != nil {
				flush("workflow run apply", scope, err)
			}
			if applied {
				return
			}
		}
	}
	d.flushWorkflowRunsForSHA(ctx, scope, owner, repo, headSHA)
}

// applyOrFlushBranchesList rewrites a tip-moved branch's entry in place from the push's `after`; only a
// create/delete (page membership) or a payload naming no ref falls back to the flush.
func (d *WebhookDispatcher) applyOrFlushBranchesList(ctx context.Context, scope, owner, repo, refName, after string, isTag bool) {
	// A tag is not a branch: it never appears in the listing, so a tag push
	// leaves the pages correct and there is nothing to do either way.
	if isTag {
		return
	}
	if refName != "" {
		applied, err := d.store.ApplyPushedBranchTip(ctx, owner, repo, refName, after, time.Now(), ghdata.BranchesCacheTTL)
		if err != nil {
			flush("branches list tip apply", scope, err)
		}
		if applied {
			return
		}
	}
	flush("branches list cache", scope, d.store.InvalidateBranchesListCache(ctx, owner, repo))
}
