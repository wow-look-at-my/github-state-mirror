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
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// WebhookDispatcher maps webhook events to cache applies / freshness invalidations.
type WebhookDispatcher struct {
	mgr   *freshness.Manager
	store *ghdata.Store
	// app, when configured, lets the dispatcher pull an as-yet-uncached repo on
	// demand (minting an installation token from the delivery's installation.id)
	// so the first webhook for a repo bootstraps a cache scope. nil disables the
	// on-demand pull — deliveries for uncached repos are then skipped.
	app *ghclient.AppAuthenticator
}

func NewWebhookDispatcher(mgr *freshness.Manager, store *ghdata.Store, app *ghclient.AppAuthenticator) *WebhookDispatcher {
	return &WebhookDispatcher{mgr: mgr, store: store, app: app}
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
	// Cached-route invalidation runs alongside (never instead of) the normal
	// apply/invalidate logic, and is deliberately disposition-neutral: it is
	// best-effort bookkeeping for the trimmed response caches and must not
	// change what the delivery reports.
	d.invalidateResponseCaches(ctx, event)

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
	default:
		return ignored("event type not tracked")
	}
}

// onWorkflowJob records GitHub Actions job state in the global workflow_jobs
// table as it happens. Only in_progress and completed are tracked; the queued
// and waiting actions are deliberately dropped (high-volume churn with no state
// worth keeping). Unlike the per-actor cache appliers there is no actor listing
// and no on-demand pull here: the table is global (webhook-fed telemetry with
// no per-credential fetch path), so the write is a single cheap upsert. Nothing
// is invalidated on a bad payload — no cached resource depends on job state.
func (d *WebhookDispatcher) onWorkflowJob(ctx context.Context, event webhook.Event) outcome {
	if event.Action != "in_progress" && event.Action != "completed" {
		return ignored("workflow_job action " + event.Action + " not tracked")
	}
	payload, err := webhook.ParseWorkflowJobPayload(event.Raw)
	if err != nil {
		slog.Warn("webhook: failed to parse workflow_job payload", "error", err)
		return ignored("unparseable workflow_job payload")
	}
	if err := d.store.RecordWorkflowJob(ctx, ghdata.WorkflowJob{
		Owner:        payload.Owner,
		Repo:         payload.Repo,
		JobID:        payload.JobID,
		RunID:        payload.RunID,
		RunAttempt:   payload.RunAttempt,
		Name:         payload.Name,
		WorkflowName: payload.WorkflowName,
		Status:       payload.Status,
		Conclusion:   payload.Conclusion,
		HeadSHA:      payload.HeadSHA,
		HeadBranch:   payload.HeadBranch,
		HTMLURL:      payload.HTMLURL,
		StartedAt:    payload.StartedAt,
		CompletedAt:  payload.CompletedAt,
		RunnerName:   payload.RunnerName,
	}); err != nil {
		slog.Warn("webhook: record workflow job failed", "repo", payload.Owner+"/"+payload.Repo, "job", payload.JobID, "error", err)
		return errored("record workflow job failed")
	}
	detail := fmt.Sprintf("job %q %s", payload.Name, payload.Status)
	if payload.Conclusion != "" {
		detail += " (" + payload.Conclusion + ")"
	}
	return applied(detail, 0)
}

func (d *WebhookDispatcher) onPush(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParsePushPayload(event.Raw)
	if err != nil {
		return d.invalidateRepoOrg(ctx, event, "unparseable push payload")
	}
	actors, err := d.actorsForRepo(ctx, event, payload.Owner, payload.Repo)
	if err != nil {
		slog.Warn("webhook: list actors for push failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
		return errored("list actors failed")
	}
	if len(actors) == 0 {
		return skipped("no cached scope for " + payload.Owner + "/" + payload.Repo)
	}
	d.absorbPushCommits(ctx, actors, payload)
	// A branch push makes GitHub recompute mergeability for every open PR
	// based on (or heading from) that branch, and no webhook ever carries the
	// recomputed value -- so un-resolve the cached mergeable rather than let
	// the single-PR cache keep serving the pre-push answer. Best-effort and
	// disposition-neutral, like the commit absorption above.
	if branch := payload.Branch(); branch != "" {
		if err := d.store.NullPRMergeableForBranchForActors(ctx, actors, payload.Owner, payload.Repo, branch); err != nil {
			slog.Warn("webhook: un-resolve PR mergeable failed", "repo", payload.Owner+"/"+payload.Repo, "branch", branch, "error", err)
		}
	}
	if err := d.store.SetRepoPushedAtForActors(ctx, actors, payload.Owner, payload.Repo, payload.PushedAt); err != nil {
		slog.Warn("webhook: apply push failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
		return errored("apply push failed")
	}
	return applied("updated pushed_at", len(actors))
}

// absorbPushCommits upserts the pushed commits into the git-commits cache for
// every actor that has the repo cached, so a subsequent
// GET /repos/{o}/{r}/git/commits/{sha} hits without any GitHub fetch ever
// having happened. The push payload states each commit's id, tree, message,
// timestamp, and author/committer -- exactly the state the endpoint returns --
// and parents come from the payload's linear chain (ChainedCommits declines
// forced/new-ref/possibly-truncated pushes rather than derive wrong parents).
// Best-effort and disposition-neutral: a failure is logged, never reported.
func (d *WebhookDispatcher) absorbPushCommits(ctx context.Context, actors []string, payload webhook.PushPayload) {
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
			// The payload states one identity timestamp; GitHub's git-commit
			// object dates author and committer separately, and for the pushed
			// commits webhooks describe they are the same wall-clock instant.
			AuthorName: c.AuthorName, AuthorEmail: c.AuthorEmail, AuthorDate: c.Timestamp,
			CommitterName: c.CommitterName, CommitterEmail: c.CommitterEmail, CommitterDate: c.Timestamp,
			TreeSHA: c.TreeID,
			Parents: []string{strings.ToLower(payload.ParentForChained(chain, i))},
		})
	}
	if err := d.store.UpsertGitCommitsForActors(ctx, actors, commits, time.Now()); err != nil {
		slog.Warn("webhook: absorb push commits failed", "repo", owner+"/"+repo, "error", err)
		return
	}
	slog.Info("webhook: absorbed push commits into git-commit cache",
		"repo", owner+"/"+repo, "commits", len(commits), "actors", len(actors))
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

// invalidateResponseCaches drops trimmed-response-cache rows a webhook makes
// stale. Deletes span ALL actors (the event is a global fact about the repo or
// installation). push/repository events flush the repo's contents rows -- the
// conservative whole-repo flush; the payload's modified-paths refinement can
// come later. repository events (rename/delete/visibility) additionally flush
// the repo's "open-PR list complete" markers; pull_request events deliberately
// do NOT -- they maintain the PR rows, which is what the marker asserts.
// installation events flush the installation's cached token mints AND cached
// repo-installation answers (a suspended/deleted/re-scoped installation must
// not keep serving either). Git-commit rows are immutable and are deliberately
// never invalidated.
func (d *WebhookDispatcher) invalidateResponseCaches(ctx context.Context, event webhook.Event) {
	switch event.Type {
	case "push", "repository":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		if err := d.store.InvalidateContentsCache(ctx, owner, repo); err != nil {
			slog.Warn("webhook: invalidate contents cache failed", "repo", owner+"/"+repo, "error", err)
		}
		if event.Type == "repository" {
			if err := d.store.InvalidatePullsListMarkers(ctx, owner, repo); err != nil {
				slog.Warn("webhook: invalidate pulls list markers failed", "repo", owner+"/"+repo, "error", err)
			}
		}
	case "installation", "installation_repositories":
		if event.InstallationID == 0 {
			return
		}
		id := fmt.Sprintf("%d", event.InstallationID)
		if err := d.store.InvalidateInstallTokenCache(ctx, id); err != nil {
			slog.Warn("webhook: invalidate install token cache failed", "installation", id, "error", err)
		}
		if err := d.store.InvalidateRepoInstallationCache(ctx, event.InstallationID); err != nil {
			slog.Warn("webhook: invalidate repo installation cache failed", "installation", id, "error", err)
		}
	}
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

	actors, err := d.actorsForRepo(ctx, event, owner, repo)
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
	actors, err := d.actorsForRepo(ctx, event, payload.Owner, payload.Repo)
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
	actors, err := d.actorsForRepo(ctx, event, payload.Owner, payload.Repo)
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

// actorsForRepo returns the actors that have this repo cached. When none do, it
// pulls the repo on demand — fetching the owner's repos once, as the GitHub App
// installation named in the delivery — so a webhook for an as-yet-uncached repo
// bootstraps a scope instead of being dropped. One fetch seeds every repo the
// installation can see, so subsequent deliveries for that org apply directly.
// Pulling is best-effort: if no app is configured, the delivery carries no
// installation, or the fetch fails, the repo simply stays uncached and the
// caller skips.
func (d *WebhookDispatcher) actorsForRepo(ctx context.Context, event webhook.Event, owner, repo string) ([]string, error) {
	actors, err := d.store.ActorsForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	if len(actors) > 0 {
		return actors, nil
	}
	if !d.pullOnDemand(ctx, event, owner) {
		return actors, nil
	}
	return d.store.ActorsForRepo(ctx, owner, repo)
}

// pullOnDemand fetches owner's repos into the delivery's app-installation
// partition, returning whether the fetch ran. It reports true only when the
// fetch actually completed, so the caller knows a re-query is worthwhile.
func (d *WebhookDispatcher) pullOnDemand(ctx context.Context, event webhook.Event, owner string) bool {
	if d.app == nil || event.InstallationID == 0 || owner == "" {
		return false
	}
	token, err := d.app.InstallationToken(ctx, event.InstallationID)
	if err != nil {
		slog.Warn("webhook: on-demand pull: mint installation token failed",
			"installation", event.InstallationID, "owner", owner, "error", err)
		return false
	}
	pctx := ghclient.WithToken(ctx, token)
	pctx = actor.WithActor(pctx, AppInstallationActor(event.InstallationID))
	if err := d.mgr.EnsureFresh(pctx, freshness.ResourceID{Kind: KindOrgRepos, Key: owner}); err != nil {
		slog.Warn("webhook: on-demand pull org repos failed",
			"installation", event.InstallationID, "owner", owner, "error", err)
		return false
	}
	slog.Info("webhook: pulled org repos on demand", "installation", event.InstallationID, "owner", owner)
	return true
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
