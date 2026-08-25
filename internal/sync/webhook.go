package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

type WebhookDispatcher struct {
	mgr      invalidator
	store    *ghdata.Store
	ordering *OrderingStats
	reorder  *reorderBuffer
}

// invalidator is the one freshness operation the dispatcher needs; narrow so tests can fake it.
type invalidator interface {
	InvalidateAllActors(ctx context.Context, kind, key string) error
}

// NewWebhookDispatcher has no reorder window; production uses NewWebhookDispatcherWindowed instead.
func NewWebhookDispatcher(mgr invalidator, store *ghdata.Store) *WebhookDispatcher {
	return NewWebhookDispatcherWindowed(mgr, store, 0)
}

// NewWebhookDispatcherWindowed sorts same-subject deliveries within window and applies them oldest-first.
// see docs/webhooks/ordering.md
func NewWebhookDispatcherWindowed(mgr invalidator, store *ghdata.Store, window time.Duration) *WebhookDispatcher {
	stats := NewOrderingStats()
	return &WebhookDispatcher{mgr: mgr, store: store, ordering: stats, reorder: newReorderBuffer(window, stats)}
}

// Ordering exposes what the out-of-order gate has seen, for the dashboard.
func (d *WebhookDispatcher) Ordering() OrderingSnapshot { return d.ordering.Snapshot() }

// outcome is a handler's result: a disposition plus a human-readable detail.
type outcome struct {
	disposition string
	detail      string
}

func applied(detail string) outcome { return outcome{disposition: webhook.DispApplied, detail: detail} }
func ignored(detail string) outcome { return outcome{disposition: webhook.DispIgnored, detail: detail} }
func errored(detail string) outcome { return outcome{disposition: webhook.DispError, detail: detail} }

// Dispatch processes a webhook event, applying it to global truth, and returns
// what it did. It also records the delivery in the global webhook log so the
// dashboard can show whether data was preserved.
func (d *WebhookDispatcher) Dispatch(ctx context.Context, event webhook.Event) webhook.DispatchResult {
	// A delivery whose subject already has a reorder batch open joins it and dispatches in clock order.
	if order, ok := webhook.OrderOf(event); ok {
		if res, buffered := d.reorder.admit(ctx, order.Subject, order.At, event, d.dispatchNow); buffered {
			return res
		}
	}
	return d.dispatchNow(ctx, event)
}

// dispatchNow processes one delivery, past any reordering hold.
func (d *WebhookDispatcher) dispatchNow(ctx context.Context, event webhook.Event) webhook.DispatchResult {
	slog.Info("webhook dispatch", "type", event.Type, "action", event.Action, "repo", event.RepoFullName())

	out := d.handle(ctx, event)

	result := webhook.DispatchResult{
		Event:       event.Type,
		Action:      event.Action,
		Repo:        event.RepoFullName(),
		Disposition: out.disposition,
		Detail:      out.detail,
	}

	// Record the delivery (best-effort: never fail the delivery over logging).
	if err := d.store.RecordWebhookDelivery(ctx, ghdata.WebhookDelivery{
		DeliveryID:  event.DeliveryID,
		EventType:   event.Type,
		Action:      event.Action,
		Repo:        result.Repo,
		Disposition: out.disposition,
		Detail:      out.detail,
	}); err != nil {
		slog.Warn("webhook: record delivery failed", "error", err)
	}

	return result
}

// handle routes an event to its handler, returning the outcome.
func (d *WebhookDispatcher) handle(ctx context.Context, event webhook.Event) outcome {
	// Order first: a superseded view must not apply or invalidate.
	// see docs/webhooks/ordering.md
	if superseded, out := d.checkOrder(ctx, event); superseded {
		return out
	}

	// Response-cache invalidation is best-effort and disposition-neutral; it never changes what the delivery reports.
	d.invalidateResponseCaches(ctx, event)

	// Keeps the repos row current from the payload; global truth learns a repo from its first webhook, no fetch needed.
	d.absorbRepoFromPayload(ctx, event)

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
	case "workflow_job":
		return d.onWorkflowJob(ctx, event)
	case "workflow_run":
		return d.onWorkflowRun(ctx, event)
	default:
		return ignored("event type not tracked")
	}
}

// absorbRepoFromPayload upserts the repos row from the delivery's repository
// object (when present). This is how a never-fetched repo enters global truth
// -- and how visibility stays webhook-fresh for the reveal layer's public fast
// path. Deleted-repo events are excluded (the row is about to be removed).
func (d *WebhookDispatcher) absorbRepoFromPayload(ctx context.Context, event webhook.Event) {
	if event.Type == "repository" && event.Action == "deleted" {
		return
	}
	repo, ok := webhook.ParseRepositoryPayload(event.Raw)
	if !ok {
		return
	}
	if err := d.store.UpsertRepo(ctx, repo); err != nil {
		slog.Warn("webhook: absorb repository object failed", "repo", event.RepoFullName(), "error", err)
	}
}

func (d *WebhookDispatcher) onPush(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParsePushPayload(event.Raw)
	if err != nil {
		// An unparseable push still proves something moved, so un-resolve mergeable repo-wide first.
		if owner, name := event.RepoOwner(), event.RepoName(); owner != "" && name != "" {
			if nerr := d.store.NullPRMergeableByRepo(ctx, owner, name); nerr != nil {
				slog.Warn("webhook: repo-wide un-resolve PR mergeable failed", "repo", owner+"/"+name, "error", nerr)
			}
		}
		return d.invalidateRepoOrg(ctx, event, "unparseable push payload")
	}
	// A branch push makes GitHub recompute mergeability for every open PR
	// based on (or heading from) that branch, and no webhook ever carries the
	// recomputed value -- so un-resolve the cached mergeable (remembering the
	// invalidated test-merge sha: a refetch re-offering it is presumed
	// pre-push -- plus the push's after tip, the proof by which an answer
	// that already reflects this push is recognized and accepted) rather than
	// let the single-PR cache keep serving the pre-push answer. This
	// invalidation runs FIRST, before any other fallible step, so a transient
	// error elsewhere in the handler can never skip it -- each step logs its
	// own failure and the handler carries on.
	if branch := payload.Branch(); branch != "" {
		if err := d.store.NullPRMergeableByBranch(ctx, payload.Owner, payload.Repo, branch, payload.After, time.Now()); err != nil {
			slog.Warn("webhook: un-resolve PR mergeable failed", "repo", payload.Owner+"/"+payload.Repo, "branch", branch, "error", err)
		}
		// A push to the default branch also stales default_branch_status; the next check event repopulates it.
		if d.isDefaultBranch(ctx, event, payload.Owner, payload.Repo, branch) {
			if err := d.store.SetRepoDefaultBranchStatus(ctx, payload.Owner, payload.Repo, sql.NullString{}); err != nil {
				slog.Warn("webhook: un-resolve default branch status failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
			}
		}
	}
	d.absorbPushCommits(ctx, payload)
	if err := d.store.SetRepoPushedAt(ctx, payload.Owner, payload.Repo, payload.PushedAt); err != nil {
		slog.Warn("webhook: apply push failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
		return errored("apply push failed")
	}
	return applied("updated pushed_at")
}

// isDefaultBranch reports whether branch is the repo's default branch,
// preferring the payload's own repository.default_branch (push payloads carry
// it) and falling back to the cached repo row (absorbed moments ago, or by an
// earlier sync). Unknown reads as false -- never null a status on a guess.
func (d *WebhookDispatcher) isDefaultBranch(ctx context.Context, event webhook.Event, owner, repo, branch string) bool {
	if branch == "" {
		return false
	}
	if r, ok := webhook.ParseRepositoryPayload(event.Raw); ok && r.DefaultBranch.Valid && r.DefaultBranch.String != "" {
		return r.DefaultBranch.String == branch
	}
	row, err := d.store.GetRepo(ctx, owner, repo)
	if err != nil {
		return false
	}
	return row.DefaultBranch.Valid && row.DefaultBranch.String == branch
}

// absorbPushCommits upserts the pushed commits into the global git-commits
// cache, so a subsequent GET /repos/{o}/{r}/git/commits/{sha} hits without any
// GitHub fetch ever having happened. The push payload states each commit's id,
// tree, message, timestamp, and author/committer -- exactly the state the
// endpoint returns -- and parents come from the payload's linear chain
// (ChainedCommits declines forced/new-ref/possibly-truncated pushes rather
// than derive wrong parents). Best-effort and disposition-neutral: a failure
// is logged, never reported.
func (d *WebhookDispatcher) absorbPushCommits(ctx context.Context, payload webhook.PushPayload) {
	chain := payload.ChainedCommits()
	if len(chain) == 0 {
		return
	}
	owner := ghdata.NormalizeRepoKey(payload.Owner)
	repo := ghdata.NormalizeRepoKey(payload.Repo)
	commits := make([]ghdata.CachedGitCommit, 0, len(chain))
	for i, c := range chain {
		if !fullHexSHA(c.ID) || c.TreeID == "" {
			return // malformed payload; absorb nothing rather than partial state
		}
		commits = append(commits, ghdata.CachedGitCommit{
			Owner: owner, Repo: repo, SHA: strings.ToLower(c.ID), Message: c.Message,
			// One payload timestamp serves both dates: webhooks report author and committer as the same instant.
			AuthorName: c.AuthorName, AuthorEmail: c.AuthorEmail, AuthorDate: c.Timestamp,
			CommitterName: c.CommitterName, CommitterEmail: c.CommitterEmail, CommitterDate: c.Timestamp,
			TreeSHA: c.TreeID,
			Parents: []string{strings.ToLower(payload.ParentForChained(chain, i))},
		})
	}
	if err := d.store.UpsertGitCommits(ctx, commits, time.Now()); err != nil {
		slog.Warn("webhook: absorb push commits failed", "repo", owner+"/"+repo, "error", err)
		return
	}
	slog.Info("webhook: absorbed push commits into git-commit cache",
		"repo", owner+"/"+repo, "commits", len(commits))
}

// fullHexSHA reports whether s is a full-length hex object id.
func fullHexSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func (d *WebhookDispatcher) onStatusChange(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParseCheckPayload(event.Type, event.Raw)
	if err != nil {
		return d.invalidateRepoOrg(ctx, event, "unparseable check payload")
	}
	// A non-completed check_suite records nothing: GitHub auto-creates a permanent-ghost PENDING
	// suite for every app with checks:write, even one that never runs a check on the sha.
	// see docs/webhooks/dispatch.md
	if event.Type == "check_suite" && payload.State == "PENDING" {
		return ignored(fmt.Sprintf("pending %s not recorded: an empty auto-created suite never completes, and real pending state rides check_run/status events", payload.Context))
	}
	rollup, err := d.store.ApplyCommitStatus(ctx, payload.Owner, payload.Repo, payload.SHA, payload.Context, payload.State, payload.OnDefaultBranch)
	if err != nil {
		slog.Warn("webhook: apply commit status failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
		return errored("apply commit status failed")
	}
	slog.Info("webhook: applied commit status",
		"repo", payload.Owner+"/"+payload.Repo, "sha", payload.SHA, "context", payload.Context,
		"rollup", rollup, "defaultBranch", payload.OnDefaultBranch)
	return applied(fmt.Sprintf("%s=%s, rollup=%s", payload.Context, payload.State, rollup))
}

// onRepository applies repository lifecycle events directly to global truth.
// The generic absorbRepoFromPayload above already upserted the current
// repository object (covering created/edited/publicized/privatized/archived/
// unarchived, since the payload carries the post-change state); this handler
// adds the destructive cases and the grant-freshness nudges.
func (d *WebhookDispatcher) onRepository(ctx context.Context, event webhook.Event) outcome {
	owner, name := event.RepoOwner(), event.RepoName()
	if owner == "" || name == "" {
		return ignored("repository event missing owner/name")
	}

	switch event.Action {
	case "deleted":
		if err := d.store.DeleteRepoCascade(ctx, owner, name); err != nil {
			slog.Warn("webhook: delete repo failed", "repo", owner+"/"+name, "error", err)
			return errored("delete repo failed")
		}
		d.invalidate(ctx, KindOrgRepos, owner)
		return applied("removed deleted repo " + owner + "/" + name)

	case "renamed":
		// The payload's repository object carries the NEW name (already
		// upserted); changes.repository.name.from names the old row, whose
		// dependents are now orphaned truth -- drop them.
		if from := webhook.ParseRenameFrom(event.Raw); from != "" && from != name {
			if err := d.store.DeleteRepoCascade(ctx, owner, from); err != nil {
				slog.Warn("webhook: delete renamed-away repo failed", "repo", owner+"/"+from, "error", err)
			}
		}
		d.invalidate(ctx, KindOrgRepos, owner)
		return applied("renamed repo; upserted " + owner + "/" + name)

	case "privatized", "publicized":
		// absorbRepoFromPayload already stored it; make the flip explicit against a degenerate payload.
		vis := ghdata.VisibilityPrivate
		if event.Action == "publicized" {
			vis = ghdata.VisibilityPublic
		}
		if err := d.store.SetRepoVisibility(ctx, owner, name, vis); err != nil {
			slog.Warn("webhook: set visibility failed", "repo", owner+"/"+name, "error", err)
			return errored("set visibility failed")
		}
		return applied("visibility -> " + vis)

	case "transferred":
		// The new owner's object was upserted already; nudge both sides' syncs so grants and truth re-converge.
		d.invalidate(ctx, KindOrgRepos, owner)
		return applied("transferred repo; upserted under " + owner)

	default:
		// created/edited/archived/unarchived: the generic absorb above already applied it;
		// fall back only if it couldn't parse a repository object.
		if _, ok := webhook.ParseRepositoryPayload(event.Raw); !ok {
			return d.invalidateRepoOrg(ctx, event, "repository payload missing repository object")
		}
		return applied("upserted repo " + owner + "/" + name)
	}
}

// onOrgChange handles organization/membership events. They change WHO can see
// what (not what is true), so the response is to mark every principal's
// org-repos sync marker for the org stale: each principal's next read re-syncs
// their grant set with their own token.
func (d *WebhookDispatcher) onOrgChange(ctx context.Context, event webhook.Event) outcome {
	if event.OrgLogin == "" {
		return ignored("org event missing login")
	}
	d.invalidate(ctx, KindOrgRepos, event.OrgLogin)
	return outcome{disposition: webhook.DispInvalidated, detail: "membership change; marked principals' org syncs stale"}
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
	switch payload.Action {
	case "deleted":
		if err := d.store.DeletePRLabelByName(ctx, payload.Owner, payload.Repo, payload.Name); err != nil {
			slog.Warn("webhook: apply label failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
			return errored("delete label failed")
		}
		return applied("removed label " + payload.Name)
	case "edited":
		if err := d.store.RecolorPRLabel(ctx, payload.Owner, payload.Repo, payload.Name, payload.Color); err != nil {
			slog.Warn("webhook: apply label failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
			return errored("recolor label failed")
		}
		return applied("recolored label " + payload.Name)
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
// mark every principal's org-repos sync for the owner stale so the next reads
// re-fetch (refreshing truth as a side effect). When the owner is unknown
// there is nothing to invalidate, so the delivery is a no-op.
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
