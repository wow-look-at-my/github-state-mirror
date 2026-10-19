package sync

import (
	"context"
	"encoding/json"
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
		flush("readme cache", scope, d.store.InvalidateReadmeCache(ctx, owner, repo))
		flush("workflows cache", scope, d.store.InvalidateWorkflowsCache(ctx, owner, repo))
		flush("owner repos cache", scope, d.store.InvalidateOwnerReposCache(ctx, owner))
		flush("commits list cache", scope, d.store.InvalidateCommitsListCache(ctx, owner, repo))
		d.flushCommitsAnswers(ctx, scope, owner, repo, nil)
		flush("compare cache", scope, d.store.InvalidateCompareCache(ctx, owner, repo))
		flush("commit CI cache", scope, d.store.InvalidateCommitCICache(ctx, owner, repo))
		flush("pull files cache", scope, d.store.InvalidatePullFilesCache(ctx, owner, repo))
		flush("branches list cache", scope, d.store.InvalidateBranchesListCache(ctx, owner, repo))
		flush("pulls list markers", scope, d.store.InvalidatePullsListMarkers(ctx, owner, repo))
		flush("closed pull cache", scope, d.store.InvalidateClosedPullCache(ctx, owner, repo))
		flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsCache(ctx, owner, repo))
		flush("workflow runs truth", scope, d.store.InvalidateWorkflowRunsTruth(ctx, owner, repo))
		flush("pull diff 406 cache", scope, d.store.InvalidatePullDiff406Cache(ctx, owner, repo))
		flush("pull diff cache", scope, d.store.InvalidateRepoAnswersOfKind(ctx, owner, repo, ghdata.RepoAnswerKindPullDiff))
		flush("git commit miss cache", scope, d.store.InvalidateGitCommitMissCache(ctx, owner, repo))
		flush("git ref cache", scope, d.store.InvalidateGitRefCache(ctx, owner, repo))
		flush("workflow jobs cache", scope, d.store.InvalidateWorkflowJobsCache(ctx, owner, repo))
		flush("pull commits cache", scope, d.store.InvalidatePullCommitsSnapshots(ctx, owner, repo))
		flush("code quality setup cache", scope, d.store.InvalidateCodeQualitySetup(ctx, owner, repo))
		flush("label cache", scope, d.store.InvalidateLabelCache(ctx, owner, repo))
		flush("hooks cache", scope, d.store.InvalidateHooksForTarget(ctx, ghdata.RepoHooksTarget(owner, repo)))
		flush("check run cache", scope, d.store.InvalidateCheckRunCacheByRepo(ctx, owner, repo))
		flush("matching refs cache", scope, d.store.InvalidateMatchingRefsCache(ctx, owner, repo))
		flush("repo answer cache", scope, d.store.InvalidateRepoAnswers(ctx, owner, repo))
	case "label":
		// Every action, repo-wide (a rename can carry names in delivery); runs before the disposition logic.
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
		flush("pull diff cache", scope, d.store.InvalidateRepoAnswersForSubject(ctx, owner, repo, ghdata.RepoAnswerKindPullDiff, strconv.FormatInt(event.PRNumber, 10)))
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
		if event.Type == "check_run" {
			d.flushCheckRunAnnotations(ctx, scope, owner, repo, event)
		}
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
			// Unparseable payload (or naming no refs): no per-ref signal, so every CI-derived cache keeps the repo-wide flush.
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
		headSHA, runID, runnerID := "", int64(0), int64(0)
		if payload, err := webhook.ParseWorkflowJobPayload(event.Raw); err == nil {
			headSHA, runID, runnerID = payload.HeadSHA, payload.RunID, payload.RunnerID
		}
		// A job start or end moves the runner's busy flag. The payload does not
		// state the rest of the runner object, so its answers drop.
		if runnerID > 0 {
			flush("runner cache", owner+"/"+repo, d.store.InvalidateRunner(ctx, owner, repo, runnerID))
		}
		d.flushWorkflowRunsForSHA(ctx, owner+"/"+repo, owner, repo, headSHA)
		// A job's state moved, and the delivery STATES it: the run's cached job answers are rewritten, flushed only where they cannot be.
		d.settleWorkflowJobs(ctx, owner+"/"+repo, owner, repo, event, runID)
	case "workflow_run":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		headSHA, runID := webhook.ParseWorkflowRunIdentity(event.Raw)
		d.settleWorkflowRuns(ctx, owner+"/"+repo, owner, repo, event, headSHA)
		d.flushWorkflowJobsForRun(ctx, owner+"/"+repo, owner, repo, runID)
		d.dropCheckRunPagesBehindCompletedRun(ctx, owner+"/"+repo, owner, repo, event)
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
// finest grain the payload supports. A push moves exactly ref, so tables
// keyed by a requested ref. Flush per-ref when the payload names it and
// repo-wide only when it does not.
func (d *WebhookDispatcher) invalidateForPush(ctx context.Context, event webhook.Event, owner, repo string) {
	scope := owner + "/" + repo
	refName, defaultBranch, before, after, isTag := "", "", "", "", false
	if payload, err := webhook.ParsePushPayload(event.Raw); err == nil {
		refName, defaultBranch, before, after = payload.RefName, payload.DefaultBranch, payload.Before, payload.After
		isTag = strings.HasPrefix(payload.Ref, "refs/tags/")
	}
	// (onPush re-parses the payload for the apply side.

	d.applyOrFlushTags(ctx, scope, owner, repo, refName, before, after, isTag)
	d.applyPushToCommitAnswers(ctx, scope, owner, repo, refName, before, after, isTag)

	switch {
	case refName == "":
		// Unparseable payload, or a ref that is neither a branch nor a tag: no per-ref signal, so every ref-relative cache flushes repo-wide.
		flush("contents cache", scope, d.store.InvalidateContentsCache(ctx, owner, repo))
		flush("readme cache", scope, d.store.InvalidateReadmeCache(ctx, owner, repo))
		flush("commits list cache", scope, d.store.InvalidateCommitsListCache(ctx, owner, repo))
		d.flushCommitsAnswers(ctx, scope, owner, repo, nil)
		flush("compare cache", scope, d.store.InvalidateCompareCache(ctx, owner, repo))
		flush("commit CI cache", scope, d.store.InvalidateCommitCICache(ctx, owner, repo))
		flush("git ref cache", scope, d.store.InvalidateGitRefCache(ctx, owner, repo))
	default:
		// Every per-ref flush below covers the pushed ref in each spelling GitHub accepts for it, since caches key rows verbatim.
		spellings := refSpellings(refName, isTag)
		// contents and commits-list rows key the REQUESTED ref. This happens where
		// the empty ref means "the default branch" -- so a default-branch push also
		// moves the empty-ref rows and flushes that spelling too. Be conservative
		// for exactly these empty-ref-keyed tables (repo-wide) rather than guess,
		// the tables with no empty-ref key stay per-ref below. Do this when the
		// payload does not say which branch is the default.
		if defaultBranch == "" {
			flush("contents cache", scope, d.store.InvalidateContentsCache(ctx, owner, repo))
			flush("readme cache", scope, d.store.InvalidateReadmeCache(ctx, owner, repo))
			flush("commits list cache", scope, d.store.InvalidateCommitsListCache(ctx, owner, repo))
			d.flushCommitsAnswers(ctx, scope, owner, repo, nil)
		} else {
			refs := spellings
			if refName == defaultBranch {
				refs = append(append([]string(nil), spellings...), "")
			}
			for _, ref := range refs {
				flush("contents cache", scope, d.store.InvalidateContentsForRef(ctx, owner, repo, ref))
				// readme rows key the requested ref as contents rows do, so they share the grain.
				flush("readme cache", scope, d.store.InvalidateReadmeForRef(ctx, owner, repo, ref))
				flush("commits list cache", scope, d.store.InvalidateCommitsListForRef(ctx, owner, repo, ref))
				d.flushCommitsAnswers(ctx, scope, owner, repo, []string{ref})
			}
		}
		for _, ref := range spellings {
			// compare rows never key an empty side, so the pushed ref's spellings are the only ones to flush, call per spelling.
			flush("compare cache", scope, d.store.InvalidateCompareForRef(ctx, owner, repo, ref))
			// commit-CI rows key the VERBATIM requested ref with no empty-ref spelling either; the pushed ref's spellings are the only ones moved.
			flush("commit CI cache", scope, d.store.InvalidateCommitCIForRef(ctx, owner, repo, ref))
			applied, err := d.store.ApplyPushedRefTip(ctx, owner, repo, ref, after, time.Now(), ghdata.GitRefCacheTTL)
			if err != nil {
				flush("git ref tip apply", scope, err)
			}
			if !applied {
				flush("git ref cache", scope, d.store.InvalidateGitRefForRef(ctx, owner, repo, ref))
			}
		}
	}

	// A push names changed FILES. A file maps to a workflow id only through the answer this caches, so repo-wide is the finest grain there is.
	flush("workflows cache", scope, d.store.InvalidateWorkflowsCache(ctx, owner, repo))

	d.applyOrFlushBranchesList(ctx, scope, owner, repo, refName, after, isTag)
	// matching_refs_cache has no narrower per-ref target than the branches listing does, so it rides the same repo-wide flush.
	flush("matching refs cache", scope, d.store.InvalidateMatchingRefsCache(ctx, owner, repo))

	// No per-ref grain for the rest, parseable or not: PR-files pages and pull-diff- verdicts both stay repo-wide.
	flush("pull files cache", scope, d.store.InvalidatePullFilesCache(ctx, owner, repo))
	flush("pull diff 406 cache", scope, d.store.InvalidatePullDiff406Cache(ctx, owner, repo))
	flush("pull diff cache", scope, d.store.InvalidateRepoAnswersOfKind(ctx, owner, repo, ghdata.RepoAnswerKindPullDiff))
	flush("pull commits cache", scope, d.store.InvalidatePullCommitsSnapshots(ctx, owner, repo))
}

// applyOrFlushTags lands a push on the stored tags pages. A moved tag is
// rewritten in place from the push's after sha. A created or deleted tag
// changes page membership, and the listing's page for a name is not in the
// payload, so the pages go. A branch push leaves them alone.
// see docs/webhooks/invalidations.md
func (d *WebhookDispatcher) applyOrFlushTags(ctx context.Context, scope, owner, repo, refName, before, after string, isTag bool) {
	if refName != "" && !isTag {
		return
	}
	if refName != "" && !zeroSHA(before) && !zeroSHA(after) {
		applied, err := d.store.ApplyPushedTagTip(ctx, owner, repo, refName, after, time.Now())
		flush("tags tip apply", scope, err)
		if err == nil && applied {
			return
		}
	}
	flush("tags cache", scope, d.store.InvalidateRepoAnswersOfKind(ctx, owner, repo, ghdata.RepoAnswerKindTags))
}

// applyPushToCommitAnswers lands a push on the single-commit and
// branches-where-head answers.
//
// A single commit asked for by a ref spelling describes whatever that ref
// points at. The payload does not carry the new commit's files or stats,
// so those rows go. A full-sha row is immutable and stays.
//
// A branches-where-head answer lists the branches whose tip a sha is. The
// branch LEFT before, so it is removed from before's answer in place. It
// JOINED after's answer, but the entry needs the branch's protected flag,
// which no push states, so after's answer goes.
// see docs/webhooks/invalidations.md
func (d *WebhookDispatcher) applyPushToCommitAnswers(ctx context.Context, scope, owner, repo, refName, before, after string, isTag bool) {
	if refName == "" {
		flush("commit answers", scope, d.store.InvalidateRepoAnswersOfKind(ctx, owner, repo, ghdata.RepoAnswerKindCommit))
		flush("branches-where-head answers", scope, d.store.InvalidateRepoAnswersOfKind(ctx, owner, repo, ghdata.RepoAnswerKindBranchesWhereHead))
		return
	}
	for _, spelling := range refSpellings(refName, isTag) {
		flush("commit answers", scope, d.store.InvalidateRepoAnswersForSubject(ctx, owner, repo, ghdata.RepoAnswerKindCommit, spelling))
	}
	if isTag {
		return
	}
	if !zeroSHA(before) {
		applied, err := d.store.RemoveBranchHead(ctx, owner, repo, before, refName, time.Now())
		flush("branches-where-head apply", scope, err)
		if err == nil && !applied {
			flush("branches-where-head answers", scope, d.store.InvalidateRepoAnswersForSubject(ctx, owner, repo, ghdata.RepoAnswerKindBranchesWhereHead, strings.ToLower(before)))
		}
	}
	if !zeroSHA(after) {
		flush("branches-where-head answers", scope, d.store.InvalidateRepoAnswersForSubject(ctx, owner, repo, ghdata.RepoAnswerKindBranchesWhereHead, strings.ToLower(after)))
	}
}

// flushCheckRunAnnotations drops the stored annotations of the check run a
// delivery names. The payload carries annotations_count and never the
// annotations, so nothing can be written. A payload naming no run drops the
// repo's annotations. see docs/webhooks/invalidations.md
func (d *WebhookDispatcher) flushCheckRunAnnotations(ctx context.Context, scope, owner, repo string, event webhook.Event) {
	var run struct {
		ID int64 `json:"id"`
	}
	if raw, ok := webhook.CheckRunObject(event.Raw); ok && json.Unmarshal(raw, &run) == nil && run.ID > 0 {
		flush("annotations", scope, d.store.InvalidateRepoAnswersForSubject(ctx, owner, repo,
			ghdata.RepoAnswerKindAnnotations, strconv.FormatInt(run.ID, 10)))
		return
	}
	flush("annotations", scope, d.store.InvalidateRepoAnswersOfKind(ctx, owner, repo, ghdata.RepoAnswerKindAnnotations))
}

// flushCommitsAnswers drops the path-filtered commits pages and the
// unknown-ref verdicts for the given ref spellings, or every one when refs is
// nil. Each rides the commits list's own grain: the payload carries no
// path-filtered history, and a created ref ends the verdict.
func (d *WebhookDispatcher) flushCommitsAnswers(ctx context.Context, scope, owner, repo string, refs []string) {
	for _, kind := range []string{ghdata.RepoAnswerKindCommitsPath, ghdata.RepoAnswerKindCommitsVerdict} {
		if refs == nil {
			flush("commits answers", scope, d.store.InvalidateRepoAnswersOfKind(ctx, owner, repo, kind))
			continue
		}
		for _, ref := range refs {
			flush("commits answers", scope, d.store.InvalidateRepoAnswersForSubject(ctx, owner, repo, kind, ref))
		}
	}
}

// zeroSHA reports the all-zeros object id a push uses for a missing side.
func zeroSHA(sha string) bool { return sha == "" || strings.Trim(sha, "0") == "" }

func flush(what, scope string, err error) {
	if err != nil {
		slog.Warn("webhook: invalidate "+what+" failed", "scope", scope, "error", err)
	}
}

// flushWorkflowRunsForSHA drops sha's cached workflow-runs pages, widening to the repo-wide flush when the sha is empty.
// see docs/webhooks/response-cache-invalidation.md
func (d *WebhookDispatcher) flushWorkflowRunsForSHA(ctx context.Context, scope, owner, repo, sha string) {
	if sha == "" {
		flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsCache(ctx, owner, repo))
		return
	}
	flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsForHeadSHA(ctx, owner, repo, sha))
}

func pullCommitsRefKey(number int64) string {
	return "pull/" + strconv.FormatInt(number, 10) + "/commits"
}

// flushWorkflowJobsForRun drops run's cached job answers -- its jobs pages
// AND the single-job rows under it -- widening to the repo-wide flush. This
// happens when the payload named no run. An id of would exact-match nothing
// (a silent no-op) while the delivery still said SOME run's jobs moved.
func (d *WebhookDispatcher) flushWorkflowJobsForRun(ctx context.Context, scope, owner, repo string, runID int64) {
	if runID <= 0 {
		flush("workflow jobs cache", scope, d.store.InvalidateWorkflowJobsCache(ctx, owner, repo))
		return
	}
	flush("workflow jobs cache", scope, d.store.InvalidateWorkflowJobsForRun(ctx, owner, repo, runID))
}

// dropCheckRunPagesBehindCompletedRun drops the check-runs pages a completed
// run proves stale. A page that lost a job's check_run delivery otherwise
// serves that job as unfinished for the whole TTL.
// see docs/webhooks/invalidations.md ("The check-run rewrite")
func (d *WebhookDispatcher) dropCheckRunPagesBehindCompletedRun(ctx context.Context, scope, owner, repo string, event webhook.Event) {
	run, err := webhook.ParseWorkflowRunPayload(event.Raw)
	if err != nil || run.Status != "completed" {
		return
	}
	dropped, err := d.store.DropCheckRunPagesContradictedByRun(ctx, owner, repo, run.HeadSHA, run.RunID)
	flush("check-runs pages behind a completed run", scope, err)
	if dropped > 0 {
		slog.Warn("webhook: completed run contradicted cached check-runs pages; a check_run delivery was lost",
			"scope", scope, "sha", run.HeadSHA, "run", run.RunID, "pages", dropped)
	}
}

// refSpellings returns every ref spelling GitHub accepts for a short branch or tag name; a per-ref flush must cover all.
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
// removed, occurrence order preserved.
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
