package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// WebhookDispatcher maps webhook events to freshness invalidations.
type WebhookDispatcher struct {
	mgr   *freshness.Manager
	store *ghdata.Store
}

func NewWebhookDispatcher(mgr *freshness.Manager, store *ghdata.Store) *WebhookDispatcher {
	return &WebhookDispatcher{mgr: mgr, store: store}
}

// Dispatch processes a webhook event and invalidates/refreshes affected resources.
func (d *WebhookDispatcher) Dispatch(ctx context.Context, event webhook.Event) {
	slog.Info("webhook dispatch", "type", event.Type, "action", event.Action, "repo", event.RepoFullName())

	switch event.Type {
	case "push":
		d.onPush(ctx, event)
	case "pull_request":
		d.onPullRequest(ctx, event)
	case "pull_request_review":
		d.onPullRequestReview(ctx, event)
	case "check_run", "check_suite", "status":
		d.onStatusChange(ctx, event)
	case "repository":
		d.onRepository(ctx, event)
	case "organization", "membership":
		d.onOrgChange(ctx, event)
	case "label":
		d.onLabel(ctx, event)
	}
}

func (d *WebhookDispatcher) onPush(ctx context.Context, event webhook.Event) {
	if owner := event.RepoOwner(); owner != "" {
		d.invalidate(ctx, KindOrgRepos, owner)
	}
}

func (d *WebhookDispatcher) onPullRequest(ctx context.Context, event webhook.Event) {
	owner, repo := event.RepoOwner(), event.RepoName()

	// Try to apply the webhook PR data directly to the DB.
	if applied := d.applyPRPayload(ctx, event); applied {
		// Data written — no need to invalidate org repos.
	} else {
		// Fallback: couldn't parse payload, invalidate the broad cache.
		if owner != "" {
			d.invalidate(ctx, KindOrgRepos, owner)
		}
	}

	// Always invalidate content-dependent caches (PR files, branch comparison)
	// because the webhook payload doesn't carry file diffs.
	if event.PRNumber > 0 && owner != "" && repo != "" {
		key := fmt.Sprintf("%s/%s/%d", owner, repo, event.PRNumber)
		d.invalidate(ctx, KindPRFiles, key)
		if event.PRBase != "" && event.PRHead != "" {
			compKey := fmt.Sprintf("%s/%s/%s...%s", owner, repo, event.PRBase, event.PRHead)
			d.invalidate(ctx, KindCompare, compKey)
		}
	}
}

// applyPRPayload parses full PR data from the webhook and writes it directly
// to the DB for all actors who have this repo cached.
func (d *WebhookDispatcher) applyPRPayload(ctx context.Context, event webhook.Event) bool {
	payload, err := webhook.ParsePRPayload(event.Raw)
	if err != nil {
		slog.Warn("webhook: failed to parse PR payload, falling back to invalidation", "error", err)
		return false
	}

	owner, repo := payload.PR.Owner, payload.PR.Repo
	if owner == "" || repo == "" {
		return false
	}

	actors, err := d.store.ActorsForRepo(ctx, owner, repo)
	if err != nil {
		slog.Warn("webhook: failed to list actors for repo", "repo", owner+"/"+repo, "error", err)
		return false
	}
	if len(actors) == 0 {
		// No actors have this repo cached — nothing to update.
		return true
	}

	// PR closed/merged → delete (we only cache open PRs).
	if payload.PR.State == "CLOSED" {
		if err := d.store.DeletePRForActors(ctx, actors, owner, repo, payload.PR.Number); err != nil {
			slog.Warn("webhook: failed to delete PR for actors", "pr", fmt.Sprintf("%s/%s#%d", owner, repo, payload.PR.Number), "error", err)
			return false
		}
		slog.Info("webhook: deleted closed PR from cache",
			"pr", fmt.Sprintf("%s/%s#%d", owner, repo, payload.PR.Number),
			"actors", len(actors))
		return true
	}

	// Open/updated PR → upsert.
	if err := d.store.UpsertPRForActors(ctx, actors, payload.PR, payload.Labels); err != nil {
		slog.Warn("webhook: failed to upsert PR for actors", "pr", fmt.Sprintf("%s/%s#%d", owner, repo, payload.PR.Number), "error", err)
		return false
	}
	slog.Info("webhook: applied PR data from webhook payload",
		"pr", fmt.Sprintf("%s/%s#%d", owner, repo, payload.PR.Number),
		"action", event.Action,
		"actors", len(actors))
	return true
}

func (d *WebhookDispatcher) onPullRequestReview(ctx context.Context, event webhook.Event) {
	if owner := event.RepoOwner(); owner != "" {
		d.invalidate(ctx, KindOrgRepos, owner)
	}
}

func (d *WebhookDispatcher) onStatusChange(ctx context.Context, event webhook.Event) {
	if owner := event.RepoOwner(); owner != "" {
		d.invalidate(ctx, KindOrgRepos, owner)
	}
}

func (d *WebhookDispatcher) onRepository(ctx context.Context, event webhook.Event) {
	if owner := event.RepoOwner(); owner != "" {
		d.invalidate(ctx, KindOrgRepos, owner)
	}
}

func (d *WebhookDispatcher) onOrgChange(ctx context.Context, event webhook.Event) {
	if event.OrgLogin != "" {
		d.invalidate(ctx, KindUserOrgs, event.OrgLogin)
	}
}

func (d *WebhookDispatcher) onLabel(ctx context.Context, event webhook.Event) {
	if owner := event.RepoOwner(); owner != "" {
		d.invalidate(ctx, KindOrgRepos, owner)
	}
}

func (d *WebhookDispatcher) invalidate(ctx context.Context, kind, key string) {
	if err := d.mgr.InvalidateAllActors(ctx, kind, key); err != nil {
		slog.Warn("webhook invalidate failed", "kind", kind, "key", key, "error", err)
	}
}
