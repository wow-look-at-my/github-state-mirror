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

// settleCommitCI lands a CI delivery on the cached commit-CI documents, at the
// grain the delivery can actually move.
//
// The two surfaces are DISJOINT upstream: a commit status never appears in a
// check-runs listing, and a check run never appears in a commit's statuses
// (measured -- a repo whose Actions produce six check runs answers its
// combined status with the one posted status and nothing else). Flushing all
// three kinds on every CI delivery therefore threw away answers the delivery
// could not have changed, on the routes the fleet polls hardest.
//
// Both delivery kinds then go further than flushing their own kinds, because
// both CARRY the thing their documents hold: a `status` rewrites the two
// status-shaped documents (SettleCommitCIFromStatus) and a `check_run`
// rewrites the check-runs listing (ApplyCheckRunToCommitCI), each dropping
// only what it cannot prove. A `check_suite` carries no runs and keeps the
// flush. docs/webhooks/invalidations.md has the ordering measurements both
// rewrites rest on.
func (d *WebhookDispatcher) settleCommitCI(ctx context.Context, scope, owner, repo string, event webhook.Event, refs []string) {
	if event.Type == "check_run" {
		// A check run is UPDATED in place upstream -- the same id moves
		// queued -> in_progress -> completed -- so the delivery states the new
		// value of one entry and the page keeps its shape.
		if raw, ok := webhook.CheckRunObject(event.Raw); ok {
			if run, ok := ghdata.TrimCheckRunJSON(raw); ok {
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

// applyMergedPRBaseTip lands a merged PR's statement about its BASE branch:
// the merge put `merge_commit_sha` on that branch, so the cached ref rows for
// it are updated from the payload rather than dropped (the apply-the-payload
// rule) -- in every spelling, since rows key the verbatim requested one.
//
// The push for that same merge says the same thing, and this is deliberately
// the SECOND way to hear it. A delivery that never arrives is this mirror's
// quietest failure and GitHub does not re-send one; a branch tip is the answer
// where that hurts most, because a consumer reading the pre-merge tip
// concludes there is nothing to do and never asks again. Two independent
// deliveries have to be lost now, not one. Ordering safety and the merged-only
// rule live in the store method and the parser.
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

// settleWorkflowJobs lands a `workflow_job` delivery on the run's cached job
// answers. The delivery carries the whole job object -- labels, runner, every
// step -- which is what those answers hold, so the job's entry is rewritten
// inside each of them and the run is flushed only where it cannot be.
//
// That is what makes a RUNNING run cacheable at all: a fetch settles which
// jobs belong to the run, and these deliveries keep their contents current.
// The flush stays for what a delivery cannot answer -- a job the stored page
// does not list (the run's membership moved), a different run_attempt, and a
// payload the model cannot hold.
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

// applyOrFlushBranchesList settles a push's effect on the cached branches
// pages. A page lists one entry per branch, and a tip-move changes only that
// entry's sha -- which the push STATES in `after` -- so the pages are
// rewritten in place rather than dropped (CLAUDE.md's apply-the-payload rule);
// dropping them re-lists every branch of the repo on the next reader, which
// for pr-minder's per-repo fork-point detection is the whole listing back over
// HTTP for a sha we were handed. Only what the pages cannot be edited into
// falls back to the flush: a create or a delete, which move page MEMBERSHIP,
// and a payload naming no ref.
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
