package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
	"github.com/wow-look-at-my/go-containers/set"
)

// invalidateResponseCaches drops trimmed-response-cache rows a webhook makes stale, at the finest per-ref/per-kind grain the payload supports. Best-effort and disposition-neutral: a failed flush is logged, never fails the dispatch.
// see docs/webhooks/response-cache-invalidation.md and docs/webhooks/invalidations.md
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
		flush("workflow runs truth", scope, d.store.InvalidateWorkflowRunsTruth(ctx, owner, repo))
		flush("pull diff 406 cache", scope, d.store.InvalidatePullDiff406Cache(ctx, owner, repo))
		flush("git commit miss cache", scope, d.store.InvalidateGitCommitMissCache(ctx, owner, repo))
		flush("git ref cache", scope, d.store.InvalidateGitRefCache(ctx, owner, repo))
		flush("workflow jobs cache", scope, d.store.InvalidateWorkflowJobsCache(ctx, owner, repo))
		flush("pull commits cache", scope, d.store.InvalidatePullCommitsSnapshots(ctx, owner, repo))
		flush("code quality setup cache", scope, d.store.InvalidateCodeQualitySetup(ctx, owner, repo))
		flush("label cache", scope, d.store.InvalidateLabelCache(ctx, owner, repo))
		flush("hooks cache", scope, d.store.InvalidateHooksForTarget(ctx, ghdata.RepoHooksTarget(owner, repo)))
		flush("check run cache", scope, d.store.InvalidateCheckRunCacheByRepo(ctx, owner, repo))
		flush("matching refs cache", scope, d.store.InvalidateMatchingRefsCache(ctx, owner, repo))
	case "label":
		// Every action, repo-wide (a rename can carry two names in one delivery); runs before the disposition logic.
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		flush("label cache", owner+"/"+repo, d.store.InvalidateLabelCache(ctx, owner, repo))
	case "pull_request", "pull_request_review":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" || event.PRNumber <= 0 {
			return
		}
		scope := prRef(owner, repo, event.PRNumber)
		flush("pull files cache", scope, d.store.InvalidatePullFilesForPR(ctx, owner, repo, event.PRNumber))
		flush("closed pull cache", scope, d.store.InvalidateClosedPullForPR(ctx, owner, repo, event.PRNumber))
		flush("pull diff 406 cache", scope, d.store.InvalidatePullDiff406ForPR(ctx, owner, repo, event.PRNumber))
		flush("pull commits cache", scope, d.store.InvalidateCommitsListForRef(ctx, owner, repo, pullCommitsRefKey(event.PRNumber)))
		if event.Type == "pull_request" {
			d.applyMergedPRBaseTip(ctx, scope, owner, repo, event)
		}
	case "status", "check_run", "check_suite":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		scope := owner + "/" + repo
		// The payload names the exact refs whose CI answers moved: the head branch(es), expanded to every spelling, plus the sha.
		// see docs/webhooks/response-cache-invalidation.md
		var refs []string
		sha := ""
		if payload, err := webhook.ParseCheckPayload(event.Type, event.Raw); err == nil {
			sha = payload.SHA
			for _, b := range payload.Branches {
				// CI-event branch names are always branches, never tags.
				refs = append(refs, refSpellings(b, false)...)
			}
			refs = dedupNonEmpty(append(refs, sha))
		}
		if len(refs) == 0 {
			// Unparseable payload (or one naming no refs): no per-ref signal, so every CI-derived cache keeps the repo-wide flush.
			flush("commit CI cache", scope, d.store.InvalidateCommitCICache(ctx, owner, repo))
			flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsCache(ctx, owner, repo))
			flush("check run cache", scope, d.store.InvalidateCheckRunCacheByRepo(ctx, owner, repo))
			return
		}
		d.settleCommitCI(ctx, scope, owner, repo, event, refs)
		// A new or finished check may also move the sha's workflow-runs listing (runs are listed per head_sha), never by branch name.
		d.flushWorkflowRunsForSHA(ctx, scope, owner, repo, sha)
		// A CI result also moves the PR's mergeable_state -- the only merge field a check event touches, since no tip moved.
		flush("PR mergeable_state", scope, d.store.NullPRMergeableStateByHeadSHA(ctx, owner, repo, sha))
	case "workflow_job":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		// Runs for EVERY workflow_job delivery, including queued/waiting ones dropped as ignored: a queued job is a run the listing may not have shown yet.
		headSHA, runID := "", int64(0)
		if payload, err := webhook.ParseWorkflowJobPayload(event.Raw); err == nil {
			headSHA, runID = payload.HeadSHA, payload.RunID
		}
		d.flushWorkflowRunsForSHA(ctx, owner+"/"+repo, owner, repo, headSHA)
		// A job's state moved, and the delivery STATES it: the run's cached job answers are rewritten, flushed only where they cannot be.
		d.settleWorkflowJobs(ctx, owner+"/"+repo, owner, repo, event, runID)
	case "workflow_run":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		// A workflow_run delivery is the ONLY signal for a startup_failure run, which creates no jobs, check runs, or statuses.
		// see docs/webhooks/response-cache-invalidation.md
		headSHA, runID := webhook.ParseWorkflowRunIdentity(event.Raw)
		d.settleWorkflowRuns(ctx, owner+"/"+repo, owner, repo, event, headSHA)
		d.flushWorkflowJobsForRun(ctx, owner+"/"+repo, owner, repo, runID)
	case "installation", "installation_repositories":
		// The app-installations LISTING is stored VERBATIM per page, so a changed installation is flushed by its app_id, not spliced in.
		if appID := webhook.ParseInstallationAppID(event.Raw); appID != "" {
			appKey := "app:" + appID
			if err := d.store.InvalidateAppInstallationsForApp(ctx, appKey); err != nil {
				slog.Warn("webhook: invalidate app installations cache failed", "app", appKey, "error", err)
			}
		}
		// "Not installed here" verdicts carry no installation id, so this delivery -- exactly the news coverage changed -- sweeps them all.
		flush("absent installation verdicts", "all apps", d.store.InvalidateAbsentRepoInstallations(ctx))
		// installation-repositories listings key a CREDENTIAL, with no installation id to match on either.
		flush("installation repos cache", "all credentials", d.store.InvalidateInstallationRepos(ctx))
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
	refName, defaultBranch, after, isTag := "", "", "", false
	if payload, err := webhook.ParsePushPayload(event.Raw); err == nil {
		refName, defaultBranch, after = payload.RefName, payload.DefaultBranch, payload.After
		isTag = strings.HasPrefix(payload.Ref, "refs/tags/")
	}
	// (onPush re-parses the payload for the apply side; the parse is a few
	// microseconds of JSON and keeping this function self-contained beats
	// threading a parsed payload through handle().)

	switch {
	case refName == "":
		// Unparseable payload, or a ref that is neither a branch nor a tag: no per-ref signal, so every ref-relative cache flushes repo-wide.
		flush("contents cache", scope, d.store.InvalidateContentsCache(ctx, owner, repo))
		flush("commits list cache", scope, d.store.InvalidateCommitsListCache(ctx, owner, repo))
		flush("compare cache", scope, d.store.InvalidateCompareCache(ctx, owner, repo))
		flush("commit CI cache", scope, d.store.InvalidateCommitCICache(ctx, owner, repo))
		flush("git ref cache", scope, d.store.InvalidateGitRefCache(ctx, owner, repo))
	default:
		// Every per-ref flush below covers the pushed ref in each spelling GitHub accepts for it, since caches key rows verbatim.
		spellings := refSpellings(refName, isTag)
		// contents and commits-list rows key the REQUESTED ref, where the
		// empty ref means "the default branch" -- so a default-branch push
		// also moves the empty-ref rows and flushes that spelling too. When
		// the payload does not say which branch is the default, be
		// conservative for exactly these two empty-ref-keyed tables
		// (repo-wide) rather than guess; the tables with no empty-ref key
		// stay per-ref below.
		if defaultBranch == "" {
			flush("contents cache", scope, d.store.InvalidateContentsCache(ctx, owner, repo))
			flush("commits list cache", scope, d.store.InvalidateCommitsListCache(ctx, owner, repo))
		} else {
			refs := spellings
			if refName == defaultBranch {
				refs = append(append([]string(nil), spellings...), "")
			}
			for _, ref := range refs {
				flush("contents cache", scope, d.store.InvalidateContentsForRef(ctx, owner, repo, ref))
				flush("commits list cache", scope, d.store.InvalidateCommitsListForRef(ctx, owner, repo, ref))
			}
		}
		for _, ref := range spellings {
			// compare rows never key an empty side, so the pushed ref's spellings are the only ones to flush, one call per spelling.
			flush("compare cache", scope, d.store.InvalidateCompareForRef(ctx, owner, repo, ref))
			// commit-CI rows key the VERBATIM requested ref with no empty-ref spelling either; the pushed ref's spellings are the only ones moved.
			flush("commit CI cache", scope, d.store.InvalidateCommitCIForRef(ctx, owner, repo, ref))
			// The ref's own tip is the one answer a push STATES outright, so it is APPLIED, not invalidated (CLAUDE.md's apply-the-payload rule).
			// see docs/webhooks/response-cache-invalidation.md
			applied, err := d.store.ApplyPushedRefTip(ctx, owner, repo, ref, after, time.Now(), ghdata.GitRefCacheTTL)
			if err != nil {
				flush("git ref tip apply", scope, err)
			}
			if !applied {
				flush("git ref cache", scope, d.store.InvalidateGitRefForRef(ctx, owner, repo, ref))
			}
		}
	}

	d.applyOrFlushBranchesList(ctx, scope, owner, repo, refName, after, isTag)
	// matching_refs_cache has no narrower per-ref target than the branches listing does, so it rides the same repo-wide flush.
	flush("matching refs cache", scope, d.store.InvalidateMatchingRefsCache(ctx, owner, repo))

	// No per-ref grain for the rest, parseable or not: PR-files pages and pull-diff-406 verdicts both stay repo-wide.
	flush("pull files cache", scope, d.store.InvalidatePullFilesCache(ctx, owner, repo))
	flush("pull diff 406 cache", scope, d.store.InvalidatePullDiff406Cache(ctx, owner, repo))
	// A PR's commit list moves when its head moves; this is the repo-wide belt for a missed pull_request delivery (a fork head's push never reaches us).
	flush("pull commits cache", scope, d.store.InvalidatePullCommitsSnapshots(ctx, owner, repo))
}

func flush(what, scope string, err error) {
	if err != nil {
		slog.Warn("webhook: invalidate "+what+" failed", "scope", scope, "error", err)
	}
}

// flushWorkflowRunsForSHA drops one sha's cached workflow-runs pages, widening to the repo-wide flush when the sha is empty.
// see docs/webhooks/response-cache-invalidation.md
func (d *WebhookDispatcher) flushWorkflowRunsForSHA(ctx context.Context, scope, owner, repo, sha string) {
	if sha == "" {
		flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsCache(ctx, owner, repo))
		return
	}
	flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsForHeadSHA(ctx, owner, repo, sha))
}

// pullCommitsRefKey mirrors the API package's synthetic commits_list_cache ref key; a sync -> api import would be a cycle, so it is restated here.
func pullCommitsRefKey(number int64) string {
	return "pull/" + strconv.FormatInt(number, 10) + "/commits"
}

// flushWorkflowJobsForRun drops one run's cached job answers -- its jobs
// pages AND the single-job rows under it -- widening to the repo-wide flush
// when the payload named no run: an id of zero would exact-match nothing (a
// silent no-op) while the delivery still said SOME run's jobs moved.
func (d *WebhookDispatcher) flushWorkflowJobsForRun(ctx context.Context, scope, owner, repo string, runID int64) {
	if runID <= 0 {
		flush("workflow jobs cache", scope, d.store.InvalidateWorkflowJobsCache(ctx, owner, repo))
		return
	}
	flush("workflow jobs cache", scope, d.store.InvalidateWorkflowJobsForRun(ctx, owner, repo, runID))
}

// refSpellings returns every ref spelling GitHub accepts for a short branch or tag name; a per-ref flush must cover all three.
func refSpellings(shortName string, isTag bool) []string {
	if shortName == "" {
		return nil
	}
	if isTag {
		return []string{shortName, "tags/" + shortName, "refs/tags/" + shortName}
	}
	return []string{shortName, "heads/" + shortName, "refs/heads/" + shortName}
}

// dedupNonEmpty returns vals with empty strings dropped and duplicates
// removed, first occurrence order preserved.
func dedupNonEmpty(vals []string) []string {
	seen := set.New[string](len(vals))
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == "" || !seen.Add(v) {
			continue
		}
		out = append(out, v)
	}
	return out
}
