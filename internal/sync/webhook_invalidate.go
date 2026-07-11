package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// invalidateResponseCaches drops trimmed-response-cache rows a webhook makes
// stale. Round 2 made the grain PER-REF where a payload names the moved refs
// -- and a per-ref flush covers every SPELLING GitHub accepts for the ref
// (refSpellings: the bare name plus heads/<name> and refs/heads/<name>, or
// the tags forms), because the caches key rows by the verbatim requested
// spelling:
//
//   - push: the pushed ref's contents/commits-list/compare/commit-CI rows
//     flush by ref (see invalidateForPush for the exact grain decisions,
//     incl. the empty-ref default-branch spelling); PR-files, branches-list,
//     and pull-diff-406 rows stay repo-wide (no per-ref signal); a payload
//     without a usable ref keeps the old conservative whole-repo flush.
//   - status/check_run/check_suite: the payload's head branch(es) + sha name
//     exactly which verbatim-ref commit-CI snapshots moved, and the sha names
//     the workflow-runs pages; an unparseable payload falls back repo-wide
//     for both, as does a (today impossible) parsed payload with no sha for
//     the workflow-runs pages.
//   - workflow_job: the job's head_sha flushes that sha's workflow-runs
//     pages (repo-wide when absent). This runs for EVERY delivery -- before
//     the disposition logic -- so queued/waiting jobs, which onWorkflowJob
//     drops as ignored, still flush (a queued job is a run the cached
//     listing may not have shown yet).
//   - workflow_run: the run's head_sha flushes that sha's workflow-runs
//     pages (repo-wide when absent). The ONLY signal for a startup_failure
//     run, which creates no jobs, check runs, or statuses -- without it a
//     runs page cached just before the failure serves stale for the full
//     TTL. The truth side has no workflow_run handler, so the delivery still
//     records as ignored (invalidation precedes disposition, the queued
//     workflow_job precedent).
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
		// The payload names the exact refs whose CI answers moved: the head
		// branch(es) -- expanded to every spelling GitHub accepts for a
		// branch, since commit_ci_cache keys the verbatim requested ref --
		// plus the sha itself. The sha flush stays single-spelling: an
		// abbreviated-sha-keyed row (which this exact-match flush would miss)
		// is bounded by the 24h TTL, and the surveyed consumers all send
		// full hex.
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
		// ParseCheckPayload requires a sha today, so this cannot be empty
		// here; if that ever relaxes, the helper widens repo-wide rather than
		// exact-matching nothing.
		d.flushWorkflowRunsForSHA(ctx, scope, owner, repo, sha)
	case "workflow_job":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		// Runs for EVERY workflow_job delivery, including the queued/waiting
		// actions onWorkflowJob drops as ignored: invalidateResponseCaches is
		// called before the disposition logic, and a queued job is exactly a
		// run the cached listing may not have shown yet.
		headSHA := ""
		if payload, err := webhook.ParseWorkflowJobPayload(event.Raw); err == nil {
			headSHA = payload.HeadSHA
		}
		d.flushWorkflowRunsForSHA(ctx, owner+"/"+repo, owner, repo, headSHA)
	case "workflow_run":
		owner, repo := event.RepoOwner(), event.RepoName()
		if owner == "" || repo == "" {
			return
		}
		// A workflow_run delivery is the ONLY signal for a run that creates
		// no jobs, check runs, or statuses -- a startup_failure (broken
		// workflow YAML) -- so without this flush a runs page cached just
		// before the failure serves stale for the full TTL. Like the
		// queued/waiting workflow_job deliveries above, invalidation runs
		// BEFORE the disposition logic: the truth side has no workflow_run
		// handler, so the delivery still records as ignored.
		d.flushWorkflowRunsForSHA(ctx, owner+"/"+repo, owner, repo,
			webhook.ParseWorkflowRunHeadSHA(event.Raw))
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
	refName, defaultBranch, isTag := "", "", false
	if payload, err := webhook.ParsePushPayload(event.Raw); err == nil {
		refName, defaultBranch = payload.RefName, payload.DefaultBranch
		isTag = strings.HasPrefix(payload.Ref, "refs/tags/")
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
		// Every per-ref flush below covers the pushed ref in each spelling
		// GitHub accepts for it (bare / heads/... / refs/heads/..., or the
		// tags forms) -- the caches key rows by the verbatim requested
		// spelling, so a bare-name-only flush would leave the qualified
		// spellings serving stale.
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
			// compare rows never key an empty side (the route guard requires
			// both sides non-empty), so the pushed ref's spellings are the
			// only ones to flush -- one call per spelling matches it on
			// either side.
			flush("compare cache", scope, d.store.InvalidateCompareForRef(ctx, owner, repo, ref))
			// commit-CI rows key the VERBATIM requested ref and have no
			// empty-ref spelling either; the pushed ref's spellings are the
			// only row families the push moves (a sha-form row describes an
			// immutable commit, and the push's brand-new shas have no rows
			// yet).
			flush("commit CI cache", scope, d.store.InvalidateCommitCIForRef(ctx, owner, repo, ref))
		}
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

// flushWorkflowRunsForSHA drops one sha's cached workflow-runs pages,
// widening to the repo-wide flush when the sha is empty: workflow_runs_cache
// keys full-hex shas, so an empty sha would exact-match nothing (a silent
// no-op) while the triggering payload still said SOME run in the repo
// changed.
func (d *WebhookDispatcher) flushWorkflowRunsForSHA(ctx context.Context, scope, owner, repo, sha string) {
	if sha == "" {
		flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsCache(ctx, owner, repo))
		return
	}
	flush("workflow runs cache", scope, d.store.InvalidateWorkflowRunsForHeadSHA(ctx, owner, repo, sha))
}

// refSpellings returns every ref spelling GitHub accepts for a short branch
// or tag name on the ref-parameterized cached routes -- the CI routes' {ref}
// segment, contents ?ref=, commits ?sha=, and compare basehead sides all
// take the bare name, the heads/<name> (tags/<name>) form, and the fully
// qualified refs/heads/<name> (refs/tags/<name>) form. The response caches
// key rows by the VERBATIM requested spelling, so a per-ref flush must cover
// all three or the alternate spellings serve stale for the full TTL.
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
