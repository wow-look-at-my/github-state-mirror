package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

func (d *WebhookDispatcher) onPullRequest(ctx context.Context, event webhook.Event) outcome {
	return d.applyPRPayload(ctx, event)
}

// applyPRPayload parses full PR data from the webhook and writes it directly
// into global truth.
func (d *WebhookDispatcher) applyPRPayload(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParsePRPayload(event.Raw)
	if err != nil {
		slog.Warn("webhook: failed to parse PR payload, falling back to invalidation", "error", err)
		return d.invalidateRepoOrg(ctx, event, "unparseable PR payload")
	}

	owner, repo := payload.PR.Owner, payload.PR.Repo
	if owner == "" || repo == "" {
		return d.invalidateRepoOrg(ctx, event, "PR payload missing owner/repo")
	}

	// PR closed/merged -> delete (we only cache open PRs).
	if payload.PR.State == "CLOSED" {
		if err := d.store.DeletePR(ctx, owner, repo, payload.PR.Number, payload.PR.UpdatedAt, time.Now()); err != nil {
			slog.Warn("webhook: failed to delete PR", "pr", prRef(owner, repo, payload.PR.Number), "error", err)
			return errored("delete closed PR failed")
		}
		// Drop the commit-check rows for the (now irrelevant) head commit.
		if err := d.store.DeleteCommitChecks(ctx, owner, repo, payload.PR.HeadRefOid.String); err != nil {
			slog.Warn("webhook: failed to delete commit checks for closed PR", "pr", prRef(owner, repo, payload.PR.Number), "error", err)
		}
		slog.Info("webhook: deleted closed PR from cache", "pr", prRef(owner, repo, payload.PR.Number))
		return applied(fmt.Sprintf("removed closed PR #%d", payload.PR.Number))
	}

	// A tip move (synchronize, base retarget) stales the payload's own retained pre-move merge fields.
	// see docs/cache/rest-routes.md (NullPRMergeableOnTipMove)
	if moved, err := d.store.NullPRMergeableOnTipMove(ctx, payload.PR, time.Now()); err != nil {
		slog.Warn("webhook: tip-move un-resolve failed", "pr", prRef(owner, repo, payload.PR.Number), "error", err)
	} else if moved {
		payload.PR.Mergeable = sql.NullString{}
		payload.PR.MergeableState = sql.NullString{}
		payload.PR.MergeCommitSha = sql.NullString{}
		slog.Info("webhook: un-resolved merge fields on tip move", "pr", prRef(owner, repo, payload.PR.Number), "action", event.Action)
	}

	// Open/updated PR -> upsert into global truth (with the CI rollup and the
	// label replace).
	if err := d.store.UpsertPRWithChecks(ctx, payload.PR, payload.Labels, time.Now()); err != nil {
		slog.Warn("webhook: failed to upsert PR", "pr", prRef(owner, repo, payload.PR.Number), "error", err)
		return errored("upsert PR failed")
	}
	slog.Info("webhook: applied PR data from webhook payload", "pr", prRef(owner, repo, payload.PR.Number), "action", event.Action)
	return applied(fmt.Sprintf("upserted PR #%d", payload.PR.Number))
}

func (d *WebhookDispatcher) onPullRequestReview(ctx context.Context, event webhook.Event) outcome {
	// The review payload embeds the full pull_request; apply it the same way.
	return d.applyPRPayload(ctx, event)
}
