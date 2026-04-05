package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// WebhookDispatcher maps webhook events to freshness invalidations.
type WebhookDispatcher struct {
	mgr *freshness.Manager
}

func NewWebhookDispatcher(mgr *freshness.Manager) *WebhookDispatcher {
	return &WebhookDispatcher{mgr: mgr}
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
	if owner := event.RepoOwner(); owner != "" {
		d.invalidate(ctx, KindOrgRepos, owner)
	}
	if event.PRNumber > 0 {
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner != "" && repo != "" {
			key := fmt.Sprintf("%s/%s/%d", owner, repo, event.PRNumber)
			d.invalidate(ctx, KindPRFiles, key)
			// Also invalidate the comparison for this PR if we have branch info.
			if event.PRBase != "" && event.PRHead != "" {
				compKey := fmt.Sprintf("%s/%s/%s...%s", owner, repo, event.PRBase, event.PRHead)
				d.invalidate(ctx, KindCompare, compKey)
			}
		}
	}
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
