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

// WebhookDispatcher applies webhook events straight to the ONE GLOBAL TRUTH
// STORE. There is no "is this repo cached for anyone?" gate and no on-demand
// pull: a webhook is GitHub telling us the one true state changed, so every
// stateful event is absorbed unconditionally -- the repos row is upserted from
// the payload's own repository object when absent. Whether any caller can
// READ the absorbed state is the reveal layer's problem, not the dispatcher's.
// (Operator directive, 2026-07-03: "just because nobody has fetched something
// doesn't mean we get to ignore updates from webhooks for it.")
type WebhookDispatcher struct {
	mgr   invalidator
	store *ghdata.Store
}

// invalidator is the one freshness operation the dispatcher needs: marking
// principals' sync markers stale after a structural change. Narrow so tests
// can fake it.
type invalidator interface {
	InvalidateAllActors(ctx context.Context, kind, key string) error
}

func NewWebhookDispatcher(mgr invalidator, store *ghdata.Store) *WebhookDispatcher {
	return &WebhookDispatcher{mgr: mgr, store: store}
}

// outcome is the internal per-handler result: a disposition (one of the
// webhook.Disp* constants) and a human-readable detail. Dispatch lifts it into
// a webhook.DispatchResult.
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
	// Cached-route invalidation runs alongside (never instead of) the normal
	// apply logic, and is deliberately disposition-neutral: it is best-effort
	// bookkeeping for the trimmed response caches and must not change what the
	// delivery reports.
	d.invalidateResponseCaches(ctx, event)

	// Keep the repos row current from the payload's own repository object.
	// Every repo-scoped payload carries full_name / private / visibility /
	// default_branch, so global truth learns about a repo from its FIRST
	// webhook -- no fetch required. Disposition-neutral and best-effort; the
	// per-event handlers below do the real work.
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

// onWorkflowJob records GitHub Actions job state in the global workflow_jobs
// table as it happens. Only in_progress and completed are tracked; the queued
// and waiting actions are deliberately dropped (high-volume churn with no state
// worth keeping). Nothing is invalidated on a bad payload -- no cached resource
// depends on job state.
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
	return applied(detail)
}

func (d *WebhookDispatcher) onPush(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParsePushPayload(event.Raw)
	if err != nil {
		return d.invalidateRepoOrg(ctx, event, "unparseable push payload")
	}
	d.absorbPushCommits(ctx, payload)
	if err := d.store.SetRepoPushedAt(ctx, payload.Owner, payload.Repo, payload.PushedAt); err != nil {
		slog.Warn("webhook: apply push failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
		return errored("apply push failed")
	}
	// A branch push makes GitHub recompute mergeability for every open PR
	// based on (or heading from) that branch, and no webhook ever carries the
	// recomputed value -- so un-resolve the cached mergeable rather than let
	// the single-PR cache keep serving the pre-push answer. Best-effort and
	// disposition-neutral, like the commit absorption above.
	if branch := payload.Branch(); branch != "" {
		if err := d.store.NullPRMergeableByBranch(ctx, payload.Owner, payload.Repo, branch); err != nil {
			slog.Warn("webhook: un-resolve PR mergeable failed", "repo", payload.Owner+"/"+payload.Repo, "branch", branch, "error", err)
		}
		// A push to the DEFAULT branch likewise stales default_branch_status:
		// the stored rollup describes the previous tip, nothing restates it
		// until the new tip's first check event, and a tip with no CI at all
		// would keep the old rollup forever (the COALESCE upsert can never
		// clear it). Un-resolve it -- the NullPRMergeableByBranch analog; the
		// next default-branch check event repopulates it.
		if d.isDefaultBranch(ctx, event, payload.Owner, payload.Repo, branch) {
			if err := d.store.SetRepoDefaultBranchStatus(ctx, payload.Owner, payload.Repo, sql.NullString{}); err != nil {
				slog.Warn("webhook: un-resolve default branch status failed", "repo", payload.Owner+"/"+payload.Repo, "error", err)
			}
		}
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
			// The payload states one identity timestamp; GitHub's git-commit
			// object dates author and committer separately, and for the pushed
			// commits webhooks describe they are the same wall-clock instant.
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

// invalidateResponseCaches drops trimmed-response-cache rows a webhook makes
// stale. Round 2 made the grain PER-REF where a payload names the moved refs:
//
//   - push: the pushed ref's contents/commits-list/compare/commit-CI rows
//     flush by ref (see invalidateForPush for the exact grain decisions,
//     incl. the empty-ref default-branch spelling); PR-files, branches-list,
//     and pull-diff-406 rows stay repo-wide (no per-ref signal); a payload
//     without a usable ref keeps the old conservative whole-repo flush.
//   - status/check_run/check_suite: the payload's head branch(es) + sha name
//     exactly which verbatim-ref commit-CI snapshots moved, and the sha names
//     the workflow-runs pages; an unparseable payload falls back repo-wide
//     for both.
//   - workflow_job: the job's head_sha flushes that sha's workflow-runs
//     pages (repo-wide when absent). This runs for EVERY delivery -- before
//     the disposition logic -- so queued/waiting jobs, which onWorkflowJob
//     drops as ignored, still flush (a queued job is a run the cached
//     listing may not have shown yet).
//   - pull_request/pull_request_review: that one PR's files pages, closed-PR
//     doc, and pull-diff-406 verdict (head pushed/synchronize -- including
//     fork heads whose pushes we never see -- base retargets, reopens).
//     Closed docs are deliberately NOT push-flushed -- a push cannot mutate
//     a closed PR, so only pull_request (per PR) and repository (whole repo)
//     events reach them.
//   - repository (rename/delete/visibility): EVERYTHING repo-wide, incl. the
//     "open-PR list complete" marker (pull_request events deliberately do
//     NOT touch it -- they maintain the PR rows, which is what the marker
//     asserts), the workflow-runs pages, the pull-diff-406 verdicts, and the
//     git-commit 404 miss markers.
//   - installation events: the installation's cached token mints AND cached
//     repo-installation answers (a suspended/deleted/re-scoped installation
//     must not keep serving either).
//
// Git-commit rows are immutable and are deliberately never invalidated; the
// git-commit 404 MISS markers are instead cleared by the absorb path itself
// (every real commit upsert un-misses its sha -- ghdata.upsertGitCommit).
// Everything here is best-effort and disposition-neutral: a failed flush is
// logged (flush), never fails the dispatch.
func (d *WebhookDispatcher) invalidateResponseCaches(ctx context.Context, event webhook.Event) {
	switch event.Type {
	case "push":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		d.invalidateForPush(ctx, event, owner, repo)
	case "repository":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		scope := owner + "/" + repo
		flush("contents cache", scope, d.store.InvalidateContentsCache(ctx, owner, repo))
		flush("commits list cache", scope, d.store.InvalidateCommitsListCache(ctx, owner, repo))
		flush("compare cache", scope, d.store.InvalidateCompareCache(ctx, owner, repo))
		flush("commit CI cache", scope, d.store.InvalidateCommitCICache(ctx, owner, repo))
		flush("pull files cache", scope, d.store.InvalidatePullFilesCache(ctx, owner, repo))
		flush("branches list cache", scope, d.store.InvalidateBranchesListCache(ctx, owner, repo))
		flush("pulls list markers", scope, d.store.InvalidatePullsListMarkers(ctx, owner, repo))
		flush("closed pull cache", scope, d.store.InvalidateClosedPullCache(ctx, owner, repo))
		flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsCache(ctx, owner, repo))
		flush("pull diff 406 cache", scope, d.store.InvalidatePullDiff406Cache(ctx, owner, repo))
		flush("git commit miss cache", scope, d.store.InvalidateGitCommitMissCache(ctx, owner, repo))
	case "pull_request", "pull_request_review":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" || event.PRNumber <= 0 {
			return
		}
		scope := prRef(owner, repo, event.PRNumber)
		flush("pull files cache", scope, d.store.InvalidatePullFilesForPR(ctx, owner, repo, event.PRNumber))
		flush("closed pull cache", scope, d.store.InvalidateClosedPullForPR(ctx, owner, repo, event.PRNumber))
		flush("pull diff 406 cache", scope, d.store.InvalidatePullDiff406ForPR(ctx, owner, repo, event.PRNumber))
	case "status", "check_run", "check_suite":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		scope := owner + "/" + repo
		// The payload names the exact ref SPELLINGS whose CI answers moved:
		// the head branch(es) plus the sha itself (commit_ci_cache keys the
		// verbatim requested ref, so branch-form and sha-form rows exist
		// independently and both must go).
		var refs []string
		sha := ""
		if payload, err := webhook.ParseCheckPayload(event.Type, event.Raw); err == nil {
			sha = payload.SHA
			refs = dedupNonEmpty(append(append([]string(nil), payload.Branches...), payload.SHA))
		}
		if len(refs) == 0 {
			// Unparseable payload (or one naming no refs): no per-ref signal,
			// so both CI-derived caches keep the conservative repo-wide flush.
			flush("commit CI cache", scope, d.store.InvalidateCommitCICache(ctx, owner, repo))
			flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsCache(ctx, owner, repo))
			return
		}
		for _, ref := range refs {
			flush("commit CI cache", scope, d.store.InvalidateCommitCIForRef(ctx, owner, repo, ref))
		}
		// A new or finished check implies the sha's workflow-runs listing may
		// have changed too (runs are listed per head_sha) -- only that sha's
		// pages, never the branch names (workflow_runs_cache keys shas only).
		flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsForHeadSHA(ctx, owner, repo, sha))
	case "workflow_job":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		scope := owner + "/" + repo
		// Runs for EVERY workflow_job delivery, including the queued/waiting
		// actions onWorkflowJob drops as ignored: invalidateResponseCaches is
		// called before the disposition logic, and a queued job is exactly a
		// run the cached listing may not have shown yet.
		headSHA := ""
		if payload, err := webhook.ParseWorkflowJobPayload(event.Raw); err == nil {
			headSHA = payload.HeadSHA
		}
		if headSHA == "" {
			flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsCache(ctx, owner, repo))
			return
		}
		flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsForHeadSHA(ctx, owner, repo, headSHA))
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

// invalidateForPush flushes the response caches a push makes stale, at the
// finest grain the payload supports: a push moves exactly ONE ref, so tables
// keyed by a requested ref flush per-ref when the payload names it and
// repo-wide only when it does not.
func (d *WebhookDispatcher) invalidateForPush(ctx context.Context, event webhook.Event, owner, repo string) {
	scope := owner + "/" + repo
	refName, defaultBranch := "", ""
	if payload, err := webhook.ParsePushPayload(event.Raw); err == nil {
		refName, defaultBranch = payload.RefName, payload.DefaultBranch
	}
	// (onPush re-parses the payload for the apply side; the parse is a few
	// microseconds of JSON and keeping this function self-contained beats
	// threading a parsed payload through handle().)

	switch {
	case refName == "":
		// Unparseable payload, or a ref that is neither a branch nor a tag:
		// no per-ref signal, so keep the pre-round-2 conservative behavior --
		// every ref-relative cache flushes repo-wide.
		flush("contents cache", scope, d.store.InvalidateContentsCache(ctx, owner, repo))
		flush("commits list cache", scope, d.store.InvalidateCommitsListCache(ctx, owner, repo))
		flush("compare cache", scope, d.store.InvalidateCompareCache(ctx, owner, repo))
		flush("commit CI cache", scope, d.store.InvalidateCommitCICache(ctx, owner, repo))
	default:
		// contents and commits-list rows key the REQUESTED ref, where the
		// empty ref means "the default branch" -- so a default-branch push
		// also moves the empty-ref rows and flushes both spellings. When the
		// payload does not say which branch is the default, be conservative
		// for exactly these two empty-ref-keyed tables (repo-wide) rather
		// than guess; the tables with no empty-ref key stay per-ref below.
		if defaultBranch == "" {
			flush("contents cache", scope, d.store.InvalidateContentsCache(ctx, owner, repo))
			flush("commits list cache", scope, d.store.InvalidateCommitsListCache(ctx, owner, repo))
		} else {
			refs := []string{refName}
			if refName == defaultBranch {
				refs = append(refs, "")
			}
			for _, ref := range refs {
				flush("contents cache", scope, d.store.InvalidateContentsForRef(ctx, owner, repo, ref))
				flush("commits list cache", scope, d.store.InvalidateCommitsListForRef(ctx, owner, repo, ref))
			}
		}
		// compare rows never key an empty side (the route guard requires
		// both sides non-empty), so the pushed ref name is the only spelling
		// to flush -- one call matches it on either side.
		flush("compare cache", scope, d.store.InvalidateCompareForRef(ctx, owner, repo, refName))
		// commit-CI rows key the VERBATIM requested ref and have no
		// empty-ref spelling either; the pushed branch/tag name is the only
		// row family the push moves (a sha-form row describes an immutable
		// commit, and the push's brand-new shas have no rows yet).
		flush("commit CI cache", scope, d.store.InvalidateCommitCIForRef(ctx, owner, repo, refName))
	}

	// No per-ref grain for the rest, parseable or not: PR-files pages (a
	// base push moves merge-base-relative file lists with no per-PR signal
	// -- the belt for missed pull_request deliveries), branches listings
	// (any push edits the listing: create, delete, tip-move), and
	// pull-diff-406 verdicts (a base push can move a PR's three-dot diff
	// across the 406 size boundary in either direction).
	flush("pull files cache", scope, d.store.InvalidatePullFilesCache(ctx, owner, repo))
	flush("branches list cache", scope, d.store.InvalidateBranchesListCache(ctx, owner, repo))
	flush("pull diff 406 cache", scope, d.store.InvalidatePullDiff406Cache(ctx, owner, repo))
}

// flush logs one best-effort cache invalidation's failure; it never fails
// the dispatch (the invalidation pass is disposition-neutral bookkeeping).
func flush(what, scope string, err error) {
	if err != nil {
		slog.Warn("webhook: invalidate "+what+" failed", "scope", scope, "error", err)
	}
}

// dedupNonEmpty returns vals with empty strings dropped and duplicates
// removed, first occurrence order preserved.
func dedupNonEmpty(vals []string) []string {
	seen := make(map[string]struct{}, len(vals))
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

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
		if err := d.store.DeletePR(ctx, owner, repo, payload.PR.Number); err != nil {
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
	// The review payload embeds the full pull_request, so apply it like a
	// pull_request event.
	return d.applyPRPayload(ctx, event)
}

func (d *WebhookDispatcher) onStatusChange(ctx context.Context, event webhook.Event) outcome {
	payload, err := webhook.ParseCheckPayload(event.Type, event.Raw)
	if err != nil {
		return d.invalidateRepoOrg(ctx, event, "unparseable check payload")
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
		// absorbRepoFromPayload already stored the new visibility (the
		// payload's repository object carries it); make the flip explicit so
		// a missing/degenerate payload object cannot leave the fast path open.
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
		// The new owner's object was upserted by absorbRepoFromPayload. The
		// old owner's row (if any) is unknown from this payload alone; nudge
		// both sides' syncs so grants and truth re-converge.
		d.invalidate(ctx, KindOrgRepos, owner)
		return applied("transferred repo; upserted under " + owner)

	default:
		// created/edited/archived/unarchived and anything else carrying a
		// repository object: the generic absorb above already applied it. A
		// payload WITHOUT a parseable repository object has applied nothing,
		// so fall back to marking syncs stale instead of claiming success.
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
