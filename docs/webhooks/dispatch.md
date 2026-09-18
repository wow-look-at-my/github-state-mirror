# Webhooks always apply (the whole point)

How each event type is applied directly to global truth. Extracted verbatim from CLAUDE.md.

## The rule: apply the payload. Invalidation is the last resort.

GitHub puts the new value in the delivery. Using it is the primary operation of this service. Deleting a row, so that a later reader buys the same fact back over HTTP, is the failure this section exists to stop. It recurs frequently, and it is the repo's signature defect. It never announces itself. The code looks tidy, the tests pass, and the cost is a needless request. The row is also simply absent for a window.

**The procedure, in order, for every handler and every cached route:**

1. **Does the payload carry the new value?** Write it. This is the answer for most events and most fields.
2. **Does it carry enough to DERIVE the new value?** Derive and write it. Two examples are parents from a push's linear commit chain, and a rollup recomputed from the checks already in truth.
3. **Only if neither** — an unparseable body, or a change whose RESULT the payload genuinely does not state — invalidate. When you do, the comment must say *what the payload cannot answer*. "Invalidate, the next reader refetches" is a bug report.

A cached route added without walking those steps is unfinished. That is the same rule the three-tier contract applies to a still-forwarding route.

### What the payloads actually hand you

| Event | Carried, therefore applicable | Genuinely not carried |
|---|---|---|
| `push` | `ref` + `after` (the ref's exact new tip), `before`, `created`/`deleted`/`forced`, `base_ref`, `repository.default_branch`, and up to **2,048** full commit objects (`id`, `tree_id`, `message`, `timestamp`, author/committer, and the added/removed/modified path lists) | file CONTENTS, comparison results, recomputed `mergeable` |
| `create` / `delete` | `ref` and `ref_type` (`branch` or `tag`), and `repository.pushed_at` as the view's clock | the ref's SHA on a create, so nothing states the new tip |
| `pull_request` | the whole PR object, its labels, base/head refs and shas | `mergeable` while GitHub recomputes (arrives `null`) — hence the COALESCE |
| `status` / `check_run` / `check_suite` | the check's name, state and head sha | — |
| `label` | the label's new name and color (an `edited` carries both names) | — |
| `repository` | visibility, name, default branch, archived/disabled | the new name's full row on a rename |
| `workflow_run` / `workflow_job` | the run/job's whole state and `head_sha` | run-level status on a job payload |

### The worked example: `git_ref_cache`

This route is the rule in miniature. It was once the rule's worst violation. A push states the ref's new tip exactly, so `invalidateForPush` **applies** it through `ghdata.ApplyPushedRefTip`. It falls back to the delete only where the payload cannot answer, which covers these cases:

- A deletion, whose `after` is all zeros. Never write that as a tip, because it means "no such ref" rather than a sha.
- A cached 404 verdict that a creation must clear. Promoting that verdict needs a `node_id` no push carries, and inventing one puts a fabricated field on the wire.
- An unreadable row.

An applied answer is byte-identical to a fetched one, pinned by `TestCachedGitRef_PushAppliesTipToEverySpelling`. It also costs no upstream calls, where the old flush cost one per spelling.

### The second one: `branches_list_cache`

A branches page lists one entry per branch, and a tip-move changes exactly that entry's sha. It changes neither the page's membership nor its ordering, because GitHub sorts by name. The push states the sha, so `ghdata.ApplyPushedBranchTip` rewrites every cached page that already lists the branch, and the pages keep serving. Only what a page cannot be edited into falls back to the repo-wide flush. That covers a create, because no page lists the branch yet, and a delete, whose `after` is all zeros. Both of those move membership. A **tag** push touches neither, because a tag is not in the listing, so it now does nothing here instead of flushing the repo's pages.

This one is worth more than the ref route it copies. pr-minder's fork-point detection lists every branch of a repo. The old flush therefore re-listed the whole repo after every push to it. `TestCachedBranchesList_PushAppliesTip` pins the applied page as byte-identical to the fetched one. That is why `marshalTrimmed` delegates to `ghdata.MarshalCacheDoc`. A doc rewritten in the storage layer and a doc rendered in the API layer must be the same bytes. One renderer is the only way to guarantee that.

### Ref lifecycle: `create` and `delete`

GitHub sends a `push` for most branch movement, but not for all of it. A tag creation and some ref lifecycle changes arrive only as `create` or `delete`. Without these events the mirror keeps serving the old ref answer for the full TTL, which is the lost-delivery failure in miniature.

`onRefLifecycle` (`internal/sync/webhook_ref.go`) reads `ref` and `ref_type` and flushes what that ref addresses. It walks every spelling of the ref, the same set `refSpellings` gives the push handler. Under each spelling it drops the git-ref, contents, commits-list, compare and commit-CI rows. It flushes the branches list and the matching-refs rows for a branch, and leaves both alone for a tag, because a tag appears in neither.

This is a step-3 invalidation. It is justified for the create half. The payload names the ref and states nothing about its sha, so no tip can be applied and no page entry can be written. The delete half is owed a conversion. The payload states that the ref is GONE, which is an answer a reader can be served, rather than a row to drop. `docs/webhooks/invalidations.md` carries that entry with the conversion named.

Ordering treats a ref lifecycle delivery as its own subject, `ref:<repo>:<ref_type>:<ref>`, clocked on `repository.pushed_at`. A create and a delete of the same tag name are therefore ordered against each other, and against nothing else.

### Still to do

- **Legitimately invalidated**, where step 3 genuinely applies: `contents_cache` and `commits_list_cache`, because a push names changed PATHS and never their contents. Also `compare_cache`, because GitHub computes a comparison rather than stating it. Also the PR merge fields, because GitHub recomputes `mergeable` asynchronously and never webhooks the result. For those fields the un-resolve, plus its stale-sha proof, IS the correct handling.

### What the dispatcher applies, per event

The dispatcher is `internal/sync/webhook.go`. It applies payloads **directly** to global truth, so a high-frequency event never triggers a GitHub re-fetch.

- `pull_request` and `pull_request_review` upsert the PR. They upsert the repos row from the payload first, so a never-fetched repo is absorbed on its first delivery.
- `status`, `check_run` and `check_suite` aggregate per-check state in the `commit_checks` table. They roll it up onto each PR's `last_commit_status`, and onto the repo's `default_branch_status` when the check ran on the default branch.
- A `check_suite` whose normalized state is PENDING is the exception. It is dropped as `ignored` and writes no row. GitHub auto-creates a suite per sha for every app holding checks:write, and an app that runs no checks leaves its empty suite queued FOREVER. The PENDING row that minted was a permanent ghost. It pinned the low-water-mark rollup and re-poisoned `last_commit_status` on every PR upsert. Nothing real is lost. A genuine suite's pending phase rides its own check runs' queued and in_progress events, and GitHub's own statusCheckRollup ignores suites. A completed suite with a real conclusion still applies. The response-cache flush still runs, because invalidation precedes the disposition logic.
- `push` updates `pushed_at` and absorbs the pushed commits into `git_commits_cache`. It **un-resolves `mergeable`** on every open PR whose base or head is the pushed branch, through `NullPRMergeableByBranch`. GitHub recomputes mergeability after either side moves, and never webhooks the result. When the pushed ref IS the default branch it **un-resolves `default_branch_status`** the same way. The stored rollup describes the previous tip. A tip with no CI otherwise keeps that rollup forever, because the COALESCE upsert can never clear it. The next default-branch check event repopulates it.
- `create` and `delete` flush the ref's cached answers, as the ref lifecycle section above describes.
- `workflow_run` applies the run's whole state to the `workflow_runs` truth table. The repo-wide runs listing is a filtered VIEW over those rows. One delivery therefore moves one run and leaves every other run served. It is never a listing flush.
- `workflow_job` maintains its run's identity and status FLOOR there, for every action including `queued`, because a queued job IS a run entering the backlog. That runs before the job table's own queued and waiting filter. A job payload carries no run-level status, so it may never settle a run.
- `label` recolors and removes labels.
- `repository` handles lifecycle. A delete calls `DeleteRepoCascade`. A rename deletes the old name and absorbs the new one. A privatize or publicize calls `SetRepoVisibility`. Any other action absorbs or invalidates.

Invalidation through `MarkStaleByKindKey` is only a **fallback**, for a structural event with an unparseable payload. Do NOT regress this into invalidate-and-refetch, and do NOT add a "who has this cached?" gate. Truth is maintained unconditionally.

`UpsertPullRequest` COALESCEs `last_commit_status` AND `mergeable` against the existing row. A PR webhook carries no CI state, and carries `mergeable: null` while GitHub is still computing it. It therefore does not clobber a known value. The un-resolve signal is the PUSH event, never the PR event's transient null. A genuinely resolved `mergeable` still overwrites. GraphQL-sourced REST-only columns are guarded the same way. A GraphQL sync therefore cannot blank what a webhook or REST absorb recorded. Those columns include `node_id`, `body`, `auto_merge_method` and `merge_commit_sha`.

`UpsertPRWithChecks` additionally derives `last_commit_status` from the commit checks already recorded for the PR's head sha. A PR opened *after* its head commit's CI finished therefore does not sit at NULL forever. That is what pr-minder's auto-opened PRs look like.

The handler dispatches **synchronously**. It returns a `DispatchResult` whose disposition is the response GitHub records. The dispositions are `applied` at 200, `ignored` and `invalidated` at 202, and `error` at 500. There is NO `skipped`. Every delivery is also written to the global `webhook_deliveries` log. Do not restore the old fire-and-forget 200.

The dispatcher also maintains the response caches, disposition-neutral and best-effort, at the finest grain each payload supports. The full per-event matrix lives in **docs/webhooks/response-cache-invalidation.md**. It states which delivery flushes which table, whether the grain is per-ref or repo-wide, and why each grain is what it is.
