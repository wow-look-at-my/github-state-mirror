package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// WebhookDispatcher maps webhook events to cache applies / freshness invalidations.
type WebhookDispatcher struct {
	mgr   *freshness.Manager
	store *ghdata.Store
}

func NewWebhookDispatcher(mgr *freshness.Manager, store *ghdata.Store) *WebhookDispatcher {
	return &WebhookDispatcher{mgr: mgr, store: store}
}

// outcome is the internal per-handler result: a disposition (one of the
// webhook.Disp* constants), a human-readable detail, and the number of cache
// scopes touched. Dispatch lifts it into a webhook.DispatchResult.
type outcome struct {
	disposition string
	detail      string
	scopes      int
}

func applied(detail string, scopes int) outcome {
	return outcome{disposition: webhook.DispApplied, detail: detail, scopes: scopes}
}
func skipped(detail string) outcome { return outcome{disposition: webhook.DispSkipped, detail: detail} }
func ignored(detail string) outcome { return outcome{disposition: webhook.DispIgnored, detail: detail} }
func errored(detail string) outcome { return outcome{disposition: webhook.DispError, detail: detail} }

// Dispatch processes a webhook event, applying it to the cache (or invalidating
// affected resources), and returns what it did. It also records the delivery in
// the global webhook log so the dashboard can show whether data was preserved.
func (d *WebhookDispatcher) Dispatch(ctx context.Context, event webhook.Event) webhook.DispatchResult {
	slog.Info("webhook dispatch", "type", event.Type, "action", event.Action, "repo", event.RepoFullName())

	out := d.handle(ctx, event)

	result := webhook.DispatchResult{
		Event:       event.Type,
		Action:      event.Action,
		Repo:        event.RepoFullName(),
		Disposition: out.disposition,
		Detail:      out.detail,
		Scopes:      out.scopes,
	}

	// Record the delivery (best-effort: never fail the delivery over logging).
	if err := d.store.RecordWebhookDelivery(ctx, ghdata.WebhookDelivery{
		DeliveryID:  event.DeliveryID,
		EventType:   event.Type,
		Action:      event.Action,
		Repo:        result.Repo,
		Disposition: out.disposition,
		Detail:      out.detail,
		Actors:      int64(out.scopes),
	}); err != nil {
		slog.Warn("webhook: record delivery failed", "error", err)
	}

	return result
}

// handle routes an event to its handler, returning the outcome.
func (d *WebhookDispatcher) handle(ctx context.Context, event webhook.Event) outcome {
	switch event.Type {
	case "push":
		return d.onPush(ctx, event)
	case "pull_request":
		return d.onPullRequest(ctx, event)
	case "pull_request_review":
		return d.onPullRequestReview(ctx, event)
	case "check_run", "check_suite", "status":
		return d.onStatusChange(ctx, event)
	case "repository":
		return d.onRepository(ctx, event)
	case "organization", "membership":
		return d.onOrgChange(ctx, event)
	case "label":
		return d.onLabel(ctx, event)
	default:
		return ignored("event type not tracked")
	}
}

func (d *WebhookDispatcher) onPush(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParsePushPayload(event.Raw)
	if err != nil {
		return d.invalidateRepoOrg(ctx, event, "unparseable push payload")
	}
	actors, err := d.store.ActorsForRepo(ctx, payload.Owner, payload.Repo)
	if err != nil {
		slog.Warn("webhook: list actors for push failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
		return errored("list actors failed")
	}
	if len(actors) == 0 {
		return skipped("no cached scope for " + payload.Owner + "/" + payload.Repo)
	}
	if err := d.store.SetRepoPushedAtForActors(ctx, actors, payload.Owner, payload.Repo, payload.PushedAt); err != nil {
		slog.Warn("webhook: apply push failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
		return errored("apply push failed")
	}
	return applied("updated pushed_at", len(actors))
}

func (d *WebhookDispatcher) onPullRequest(ctx context.Context, event webhook.Event) outcome {
	// Apply the PR payload to the webhook-fed org cache. The PR's file list and
	// branch comparison are NOT cached by the mirror anymore (those endpoints
	// passthrough to GitHub verbatim), so there is nothing content-dependent to
	// invalidate here.
	return d.applyPRPayload(ctx, event)
}

// applyPRPayload parses full PR data from the webhook and writes it directly to
// the DB for all actors who have this repo cached.
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

	actors, err := d.store.ActorsForRepo(ctx, owner, repo)
	if err != nil {
		slog.Warn("webhook: failed to list actors for repo", "repo", owner+"/"+repo, "error", err)
		return errored("list actors failed")
	}
	if len(actors) == 0 {
		return skipped("no cached scope for " + owner + "/" + repo)
	}

	// PR closed/merged → delete (we only cache open PRs).
	if payload.PR.State == "CLOSED" {
		if err := d.store.DeletePRForActors(ctx, actors, owner, repo, payload.PR.Number); err != nil {
			slog.Warn("webhook: failed to delete PR for actors", "pr", prRef(owner, repo, payload.PR.Number), "error", err)
			return errored("delete closed PR failed")
		}
		// Drop the commit-check rows for the (now irrelevant) head commit.
		if err := d.store.DeleteCommitChecksForActors(ctx, actors, owner, repo, payload.PR.HeadRefOid.String); err != nil {
			slog.Warn("webhook: failed to delete commit checks for closed PR", "pr", prRef(owner, repo, payload.PR.Number), "error", err)
		}
		slog.Info("webhook: deleted closed PR from cache", "pr", prRef(owner, repo, payload.PR.Number), "actors", len(actors))
		return applied(fmt.Sprintf("removed closed PR #%d", payload.PR.Number), len(actors))
	}

	// Open/updated PR → upsert.
	if err := d.store.UpsertPRForActors(ctx, actors, payload.PR, payload.Labels); err != nil {
		slog.Warn("webhook: failed to upsert PR for actors", "pr", prRef(owner, repo, payload.PR.Number), "error", err)
		return errored("upsert PR failed")
	}
	slog.Info("webhook: applied PR data from webhook payload", "pr", prRef(owner, repo, payload.PR.Number), "action", event.Action, "actors", len(actors))
	return applied(fmt.Sprintf("upserted PR #%d", payload.PR.Number), len(actors))
}

func (d *WebhookDispatcher) onPullRequestReview(ctx context.Context, event webhook.Event) outcome {
	// The review payload embeds the full pull_request, so apply it like a
	// pull_request event instead of invalidating the whole org.
	return d.applyPRPayload(ctx, event)
}

func (d *WebhookDispatcher) onStatusChange(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParseCheckPayload(event.Type, event.Raw)
	if err != nil {
		return d.invalidateRepoOrg(ctx, event, "unparseable check payload")
	}
	actors, err := d.store.ActorsForRepo(ctx, payload.Owner, payload.Repo)
	if err != nil {
		slog.Warn("webhook: list actors for status failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
		return errored("list actors failed")
	}
	if len(actors) == 0 {
		return skipped("no cached scope for " + payload.Owner + "/" + payload.Repo)
	}
	rollup, err := d.store.ApplyCommitStatusForActors(ctx, actors, payload.Owner, payload.Repo, payload.SHA, payload.Context, payload.State, payload.OnDefaultBranch)
	if err != nil {
		slog.Warn("webhook: apply commit status failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
		return errored("apply commit status failed")
	}
	slog.Info("webhook: applied commit status",
		"repo", payload.Owner+"/"+payload.Repo, "sha", payload.SHA, "context", payload.Context,
		"rollup", rollup, "defaultBranch", payload.OnDefaultBranch, "actors", len(actors))
	return applied(fmt.Sprintf("%s=%s, rollup=%s", payload.Context, payload.State, rollup), len(actors))
}

func (d *WebhookDispatcher) onRepository(ctx context.Context, event webhook.Event) outcome {
	owner := event.RepoOwner()
	if owner == "" {
		return ignored("repository event missing owner")
	}
	d.invalidate(ctx, KindOrgRepos, owner)
	return outcome{disposition: webhook.DispInvalidated, detail: "structural change; marked org repos stale"}
}

func (d *WebhookDispatcher) onOrgChange(ctx context.Context, event webhook.Event) outcome {
	if event.OrgLogin == "" {
		return ignored("org event missing login")
	}
	d.invalidate(ctx, KindUserOrgs, event.OrgLogin)
	return outcome{disposition: webhook.DispInvalidated, detail: "membership change; marked user orgs stale"}
}

func (d *WebhookDispatcher) onLabel(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParseLabelPayload(event.Raw)
	if err != nil {
		return d.invalidateRepoOrg(ctx, event, "unparseable label payload")
	}
	// A brand-new label definition has no cached PRs referencing it yet.
	if payload.Action == "created" {
		return ignored("new label definition; no cached PRs reference it")
	}
	// A rename touches the label's primary key across many PRs; re-fetch.
	if payload.Action == "edited" && payload.OldName != "" && payload.OldName != payload.Name {
		return d.invalidateRepoOrg(ctx, event, "label renamed")
	}
	actors, err := d.store.ActorsForRepo(ctx, payload.Owner, payload.Repo)
	if err != nil {
		slog.Warn("webhook: list actors for label failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
		return errored("list actors failed")
	}
	if len(actors) == 0 {
		return skipped("no cached scope for " + payload.Owner + "/" + payload.Repo)
	}
	switch payload.Action {
	case "deleted":
		if err := d.store.DeletePRLabelByNameForActors(ctx, actors, payload.Owner, payload.Repo, payload.Name); err != nil {
			slog.Warn("webhook: apply label failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
			return errored("delete label failed")
		}
		return applied("removed label "+payload.Name, len(actors))
	case "edited":
		if err := d.store.RecolorPRLabelForActors(ctx, actors, payload.Owner, payload.Repo, payload.Name, payload.Color); err != nil {
			slog.Warn("webhook: apply label failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
			return errored("recolor label failed")
		}
		return applied("recolored label "+payload.Name, len(actors))
	default:
		return ignored("label action " + payload.Action + " not tracked")
	}
}

func (d *WebhookDispatcher) invalidate(ctx context.Context, kind, key string) {
	if err := d.mgr.InvalidateAllActors(ctx, kind, key); err != nil {
		slog.Warn("webhook invalidate failed", "kind", kind, "key", key, "error", err)
	}
}

// invalidateRepoOrg is the fallback when a payload can't be applied directly:
// mark the owner's org-repos cache stale so the next request re-fetches. When
// the owner is unknown there is nothing to invalidate, so the delivery is a no-op.
func (d *WebhookDispatcher) invalidateRepoOrg(ctx context.Context, event webhook.Event, reason string) outcome {
	owner := event.RepoOwner()
	if owner == "" {
		return ignored(reason + "; no repo owner")
	}
	d.invalidate(ctx, KindOrgRepos, owner)
	return outcome{disposition: webhook.DispInvalidated, detail: reason + "; marked org repos stale"}
}

func prRef(owner, repo string, number int64) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}
