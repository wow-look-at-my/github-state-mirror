# Schema history

What each schema change was and why, extracted verbatim from the numbered
`SchemaVersion` block that used to live in `internal/database/db.go`.

The numbers were a hand-maintained constant, and a schema change that forgot to
move it deployed against the table it no longer described. `schemaFingerprint`
replaced them: the nuke decision is now the hash of `schema.sql` with comments
scrubbed, so a schema change always rebuilds the DB and nothing else ever does.
Numbering stopped at 26; entries below are kept for their reasoning, and new
changes are recorded in the commit that makes them.

**26: `pull_requests.mergeable_state`** — GitHub's merge-state string, which the
single-PR route parsed off upstream and then dropped. The rebuilt document is
served on the MISS path too, so the field was unreachable through this mirror on
every path: a consumer could not read it at all, and nothing said so. It is the
only place GitHub states that a strict up-to-date rule is what blocks a merge
(`behind`), so its absence pushed pr-minder onto inferring behindness from a
compare -- an answer this cache can serve stale, and did, leaving armed PRs
sitting behind their base for hours (grok-build#51). Un-resolved alongside
`mergeable` wherever a tip moves, plus on CI events, which move it and nothing
else.

**25: `pr_closures`** — the record that a PR left the cache because it closed.
Only open PRs are retained, so a close deletes the row, and an absent row cannot
lose a comparison: a delivery carrying OLDER state re-inserts the PR as open and
nothing afterwards restates the close. That is not hypothetical -- a merged PR
was resurrected 82 seconds after its merge by a redelivered pre-merge payload,
and stayed open in the cache for the next 44 minutes. The closure record lets
every open write path refuse a write that cannot prove it postdates the close.

**24: `compare_cache.base_tip_sha`** — `base_commit.sha`, the base tip GitHub
says a comparison was computed against. A cached `behind_by` is the one stale
answer that stops its own correction (pr-minder reads it to decide whether a PR
needs its branch updated, so a stale "not behind" ends the work that would
refresh it), and it used to sit until the 24h TTL if the push flush missed it.
With the tip recorded, a read can compare it against what `git_ref_cache` says
the branch is on now -- which a push keeps current by applying its own `after`
-- and refuse the row.

**23: four uncached slices become cached routes.**
`repo_installation_cache` gains status/message, so the authoritative "not
installed here" 404 becomes a cacheable VERDICT (bounded by its own short TTL,
since only OUR App's installation events reach us); `label_cache` holds the
single-label read, flushed by `label` deliveries; `installation_repos_cache`
holds `GET /installation/repositories` keyed by the BEARER's fingerprint,
because that answer belongs to one installation token rather than to the app
principal its callers share; and `hooks_cache` holds the repo and org webhook
CONFIGURATION listings, keyed by the bearer for a stronger reason -- they are
ADMIN-only reads and the reveal layer only proves READ access.

**22: `workflow_runs`** (global truth for Actions RUN state, maintained per run
by `workflow_run`/`workflow_job` deliveries) + `workflow_runs_list_cache` (the
completeness proof that lets the repo-wide runs LISTING be rebuilt from those
rows). The listing is NOT a snapshot: clearing one on every job delivery would
be invalidate-and-refetch.

**21: `code_quality_setup_cache`** — the cached Code Quality enablement read.
Config with no webhook: flushed by a PATCH the mirror proxies and by
`repository` events, otherwise bounded by a short TTL.

**20: `workflow_jobs_cache`** — the cached Actions job reads (a run's jobs page
and a single job). Only TERMINAL answers are stored; a re-run replaces a run's
jobs under the same run id, so both kinds carry `run_id` and one flush covers
everything the re-run invalidates.

**19: `git_ref_cache`** — the cached `GET /git/ref/{ref}` lookup (the hottest
UNROUTED path in the request log). Keyed by the VERBATIM requested ref spelling;
a push naming the ref flushes every spelling, which is also what clears a cached
absent-ref 404 verdict.

**18: `pull_requests` gains `merge_stale_ref`/`merge_stale_after`** — the stale
marker's push-tip PROOF. A base/head push now records WHICH branch moved and its
post-push tip alongside the remembered sha, so an absorbed answer whose reported
tip for that branch equals the push's after sha provably post-dates the push and
is accepted even when it re-offers the remembered sha -- the wrong-mark race (a
fresh post-push answer absorbed before the late push delivery landed, then
stamped stale by it) heals on the very next poll instead of wedging the row into
missing for the whole `MergeStaleTTL` window.

**17: `merge_stale_sha`/`merge_stale_at`** — the push-invalidated test-merge sha
memory: a refetch re-offering the exact nulled sha is presumed pre-push (a tip
change always changes the sha of a successful test merge) and is stored
unresolved instead of re-resolving, so the single-PR route keeps missing until
GitHub serves a fresh sha.

**16: respcache round 2.** `commit_ci_cache` gained pagination columns
(`per_page`, `page`; the unique key is now owner/repo/ref/kind/per_page/page)
and a third kind `statuses_list` (the raw statuses LIST route); `compare_cache`
gains `base_ref`/`head_ref` (the basehead's two sides, split at `...`, so a push
can flush per ref instead of per repo) and a status column (404 "unknown ref"
answers become expiring miss markers); and three new tables --
`workflow_runs_cache` (per-page snapshots of
`GET /repos/{owner}/{repo}/actions/runs?head_sha=...`),
`git_commit_miss_cache` (expiring 404 verdicts for the single git-commit read;
cleared by any real commit upsert so a sha that materializes stops answering
404), and `pull_diff406_cache` (406 "diff too large" verdicts for the single-PR
diff read; 200 diff bodies are never stored).

**15:** added `pull_files_cache`, `closed_pull_cache`, and
`branches_list_cache`.

**14: a no-schema-change nuke** of truth rows poisoned by the collaborator-repo
bleed -- repos a User login merely collaborates on, absorbed keyed by the WRONG
owner; the fixed fetch (`ownerAffiliations: OWNER` + the `dropForeignRepoNode`
guard in ghclient) keeps them out afterwards. This is the one entry the
fingerprint could not express, and deliberately so: poisoned rows are a data
problem, and the fix for one is to correct or re-fetch the rows, not to hand
every deploy a lever that empties the cache.

**13: `commit_ci_cache`** — trimmed combined-commit-status and check-runs
snapshots backing the cached `GET /repos/{owner}/{repo}/commits/{ref}/status`
and `.../commits/{ref}/check-runs` routes.

**12:** added `compare_cache` for the cached compare route.

**11:** added `commits_list_cache` for the cached commits LIST route.

**10: ONE GLOBAL TRUTH STORE**, the global-cache re-architecture: the actor
dimension dropped from every GitHub-state table, access decided at serve time by
the reveal-by-permission layer.

**9:** the per-actor `/pulls` + `/installation` cache branch, folded into that
model.

**8:** per-user partitions.

**7:** added `workflow_jobs`.

**6:** added the response-cache tables.

## After a nuke

A rebuilt DB is empty, and the periodic refresh cadence (`REFRESH_INTERVAL`,
default 6h) is what repopulates it. Run the consistency check's apply mode
(`POST /api/cache/check?apply=true`, or the Principals tab's button) once after
a deploy that changed the schema, to rebuild truth promptly instead of waiting
out that cadence.
