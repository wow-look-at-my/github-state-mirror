# Every invalidation, and whether it is defensible

CLAUDE.md's rule: **apply the payload. Invalidation is the last resort.** A delivery is the answer, not a cache-busting ping. Dropping a row and buying the same fact back over HTTP is a bug report, not a design.

This file is the complete inventory of the places this service still drops a row instead of writing one. Each place names what the payload actually carries, and a verdict. It exists so the count can only go down. An entry here is either justified with a reason that survives reading, or it is named as owed work.

Two things are deliberately NOT in this file. `docs/webhooks/payload-unused/` covers whole HANDLERS that consume no payload at all, and is a build gate (`TestWebhookHandlersConsumeTheirPayload`). This file covers the response-cache flushes in `internal/sync/webhook_invalidate.go`. Those flushes are disposition-neutral bookkeeping, and no test can classify them for you.

## Legend

- **Justified** — the payload cannot answer. The reason is specific to this row rather than a general shrug.
- **Owed** — the payload can answer and does not yet. Named, with the conversion.

## push (`invalidateForPush`)

| row family | verdict | why |
| --- | --- | --- |
| `git_ref_cache` | **applied** | the push STATES the ref's new tip (`after`); `ApplyPushedRefTip` writes it. The delete is the fallback for a deletion (all-zeros tip), a 404-verdict row a creation must clear, and an unreadable row. |
| `branches_list_cache` | **applied** | a tip move changes one entry's sha, which the payload states; the pages are rewritten in place. Only create/delete, which move page MEMBERSHIP, fall back to a flush. |
| `contents_cache` | **Justified** | the payload names the files each commit added/modified/removed. It never carries their CONTENT, and the cached answer IS the content (plus its blob sha, size and encoding). Nothing to write. |
| `commits_list_cache` | **Owed** | the payload carries up to 2,048 full commit objects, oldest-first, and the cached answer is exactly "commits from this ref, newest first". Page 1 of an unfiltered listing could be rebuilt by prepending the pushed commits. Blocked on: the rendered doc carries per-commit fields the push payload does not (author/committer `login`, `html_url`-free identity, verification), so a prepended entry would not be byte-identical to a fetched one — which the hit/miss identity rule forbids. Conversion needs those fields modelled as absent-vs-unknown first. |
| `compare_cache` | **Justified today, superseded later** | a diff is content, and no push payload carries it. The real answer is not to apply this row but to stop storing it: with the commits cached, a three-dot compare is computable locally (see "Single source of truth" below), at which point this cache and its invalidation both disappear. |
| `commit_ci_cache` (branch-form rows) | **Justified** | the rows keyed by a branch name describe "the CI of whatever that branch points at". The push moves the branch, and the payload says nothing about the new tip's checks — there are usually none yet, but a force-push can point a branch at an old commit that has a full set. Writing "no checks" would be a guess that is wrong exactly when it matters. Sha-form rows are untouched: they describe an immutable commit. |
| `readme_cache` | **Justified** | the same shape as `contents_cache` above, at the same per-ref grain. The payload names the files each commit touched. It never carries their CONTENT, and the cached answer IS the content. |
| `workflows_cache` | **Justified** | the payload names changed FILES. A map from `.github/workflows/ci.yml` to a workflow id needs the listing this table is the cache of. There is no finer grain to flush at, and nothing to write. A repo holds few workflows. |
| `pull_files_cache`, `pull_diff406_cache` | **Justified** | a base push moves every open PR's merge-base-relative file list, and the payload carries no per-PR signal at all — not even which PRs exist. The repo-wide flush is the belt for the per-PR flush that `pull_request` deliveries do carry. |
| `pull_commits` snapshots | **Justified** | same shape: a fork head's pushes never reach us, so this is the repo-wide belt for a missed `pull_request` delivery. |
| PR `mergeable` fields | **applied-as-unresolved** | GitHub recomputes mergeability asynchronously and no webhook ever carries the recomputed value, so the stored answer is un-resolved rather than dropped, with the invalidated test-merge sha remembered so a refetch re-offering it is recognized as pre-push. |

## create / delete (`onRefLifecycle`)

A ref lifecycle delivery carries `ref` and `ref_type`, and nothing else about the ref. A tag creation reaches the mirror through this event alone, never through a push.

| row family | verdict | why |
| --- | --- | --- |
| `git_ref_cache` (create) | **Justified** | the payload names the ref and states no sha for it. There is no tip to write, and a cached 404 verdict for that ref is now wrong. |
| `git_ref_cache` (delete) | **Owed** | the payload states that the ref is GONE, which is a servable answer rather than a row to drop. The conversion is to store the 404 verdict for every spelling of the ref, the shape `absorbHook` already uses for an absent hook. Blocked on the same identity rule as a create: a stored ref answer carries a `node_id` no lifecycle payload states, so the not-found document needs modelling before a written row can be byte-identical to a fetched one. |
| `branches_list_cache`, matching-refs rows | **Justified** | a create and a delete both move page MEMBERSHIP. The payload carries no sha for a created branch, so no entry can be written, and the delete case is the same fallback `invalidateForPush` already takes. A tag touches neither row family and flushes neither. |
| `contents_cache`, `commits_list_cache` | **Justified** | both are keyed by ref and both hold content. The payload carries no content, exactly as it carries none on a push. |
| `compare_cache`, `commit_ci_cache` (branch-form rows) | **Justified** | both describe whatever the ref points at. A deleted ref points at nothing, and a created one points at a sha the payload never states. |

## pull_request / pull_request_review

| row family | verdict | why |
| --- | --- | --- |
| PR row + labels | **applied** | the payload is a full REST-shaped PR; rows stay rebuild-complete from it. |
| base branch tip (`git_ref_cache`) | **applied** | a MERGED PR states the commit merging put on the base branch (`ApplyMergedBaseTip`). |
| `pull_files_cache` (this PR) | **Justified** | the payload states the head moved; it does not state the resulting file list. |
| `closed_pull_cache` (this PR) | **Justified** | the cached doc is a rendered whole-PR snapshot for a CLOSED PR; a reopen/edit/relabel changes fields the doc holds, and the payload does carry them — but the doc is only ever written for a PR the truth table no longer retains, so patching it would mean maintaining a second PR renderer against a row that does not exist. Dropping it costs one fetch on a rarely-read path. |
| `pull_diff406_cache` (this PR) | **Justified** | the verdict is "this PR's diff is too large to serve", a property of content the payload does not carry. |
| `pull_commits` (this PR) | **Owed** | a `synchronize` states the new head sha, and the cached answer is the PR's commit list. It cannot be rebuilt from the payload (the payload carries no commit objects for a PR event), so this one is only convertible together with the compare rework below. |

## status / check_run / check_suite

| row family | verdict | why |
| --- | --- | --- |
| `commit_checks` truth | **applied** | the payload's state is written directly. |
| PR `mergeable_state` | **applied-as-unresolved** | a CI result is the only thing that moves unstable/blocked ↔ clean, and no payload carries the recomputed value. `mergeable_state` is stored beside `mergeable`, and the two must resolve and un-resolve as ONE fact — a row holding a resolved `mergeable_state` next to a nulled `mergeable` serves the pre-push answer under the other field's name, the same staleness the `merge_stale_sha` guard exists to refuse, wearing a different column (`internal/ghdata/mergeable_state_test.go`). |
| `commit_ci_cache`, the `/status` and `/statuses` docs, on a `status` event | **applied** | the payload IS the status, whole. `SettleCommitCIFromStatus` rewrites both documents from it — see "The status rewrite" below for the ordering rules that make the result exact rather than plausible — and drops only what a stored page cannot provably hold. |
| `commit_ci_cache`, the `/check-runs` docs, on a `check_run` event | **applied** | the delivery carries the whole run object, which is one entry of that listing. Unlike a status, a check run is UPDATED IN PLACE upstream — the same id moves queued → in_progress → completed — so the rewrite replaces the entry where it stands. A run the page does not list yet is ADDED at its place in the id order: a creation names the commit its run belongs to, so membership GROWS from the payload. See "The check-run rewrite" below for the ordering measurement that makes both placements a fact rather than a hope. A `check_suite` delivery carries no runs and keeps the flush. |
| the OTHER kinds, on either event | **applied** (nothing to do) | a commit status never appears in a check-runs listing and a check run never appears in a commit's statuses. Flushing all three kinds on every CI delivery re-fetched answers the delivery could not have changed; each now flushes only its own surface. |
| `workflow_runs_cache` (sha pages) | **applied** on `workflow_run`, **flushed** on the rest | a `workflow_run` delivery IS the run object these pages list, so its entry is rewritten in place. A run is updated in place upstream and the listing is ordered by CREATION — measured across 100 consecutive runs of one repo: `created_at` strictly descending with zero violations, `updated_at` with 31 — so the entry cannot move, and `total_count` (GitHub's total, not the page length) does not change when a run merely changes state. A run the pages do not list is a NEW run for the sha, a membership change only a fetch settles. `status`/`check_run`/`check_suite`/`workflow_job` deliveries name the sha but not the run object, so they keep the flush. |

## workflow_job / workflow_run

| row family | verdict | why |
| --- | --- | --- |
| `workflow_jobs` truth | **applied** | job state is written from the payload. |
| `workflow_jobs_cache` (run/job pages) | **applied** | the delivery carries the whole job object — labels, runner, every step — which is exactly one entry of these answers, so `ApplyWorkflowJob` rewrites it in place. See "Live job state" below: this is also what made a RUNNING run cacheable, which was three quarters of the mirror's passthrough volume. The flush stays for what a delivery cannot answer: a job the page does not list (the run's membership moved) and a different `run_attempt` (a re-run is a different set of jobs). |

## repository

Everything repo-wide. **Justified**: a rename, delete or visibility change makes the rows WRONG rather than stale. They are keyed by a name that no longer means what it did. They also hold data a now-private repo must not serve. The payload states the repo's new identity, never the new content of every answer keyed by the old one.

`owner_repos_cache` rides the same delivery, at the OWNER grain rather than the repo grain. A created, deleted or renamed repo moves the owner's listing. **Justified**: the delivery names the repo and never the credential, and each row keys a bearer fingerprint. There is no per-token signal to patch with, so the flush spans every credential holding a listing for that owner.

## installation / installation_repositories

`install_token_cache` and `repo_installation_cache` by id, plus a sweep of every absent-installation verdict and the whole `installation_repos_cache`. **Justified**: the delivery is precisely the news that an installation's coverage changed. Neither the verdicts nor the listings carry an id to match on. A suspended installation must stop serving minted tokens immediately. Applying anything here means inventing the new coverage.

## label

Repo-wide on every action. **Justified**: an `edited` action can RENAME a label, which puts two names in one delivery. One label also answers under every spelling a caller can request. Matching the payload's name therefore misses rows. The grain is the repo because the key space is unbounded, not because the payload is thin.

## The status rewrite

Rewriting a stored document from a payload is only correct if the result matches a fetch byte for byte. A hit replays the stored bytes. For the two status-shaped documents that came down to their ORDERING, which the REST docs do not state. It was measured instead, on `kubernetes/kubernetes` at `a231bf3f`, across 53 raw statuses over 22 contexts:

- Statuses are **append-only**. Re-posting a context creates a new status object with a new id and a new `created_at`. Nothing is mutated in place. That is why the raw list keeps every one of them. The combined status keeps one per context.
- `/statuses` is **newest-first**. A new status is PREPENDED, and the context's older entries stay.
- `/status` holds the **latest per context, oldest-first**, which is exactly the raw list's per-context latest, reversed. A new status therefore leaves its context's old position and lands at the END.
- Its `state` is the documented rollup. It is failure if any context is error or failure. It is pending if there are no contexts, or any context is pending. Otherwise it is success. `total_count` is the array's own length.

Every rewrite refuses unless the stored page PROVES it can hold the result. It must be page 1, with the whole set present, with room for a new entry. The status must also be the newest on the commit, because an older one's position is unknown. A refusal drops the row, which is what happened to every one of these before the rewrite existed.

What the payload can IDENTIFY decides which rows are reached. The ref spellings the delivery names do not. A combined status states the sha it resolved to, so a branch-form row is usable and a row about another commit is left alone. The raw list is a bare array with no sha in it. Only the sha-form row is provably about this commit, so the branch spellings still flush.

`TestCachedCommitCI_StatusEventRewritesTheCombinedDoc` (internal/api) is the pin. It byte-compares a rewritten document against the fetch it saved.

## The check-run rewrite

A check run is not a status. A status is APPEND-ONLY, because re-posting a context mints a new object. A check run is UPDATED IN PLACE, because one id carries the run from queued through completed. The rewrite therefore never has to decide where an entry goes. It only has to know that the entry does not MOVE when it changes.

That was measured, not assumed, on this repo's own head commit, sampled twice across a status transition. The listing is **id-descending**. The sample that discriminates is a run with a LOWER id and a LATER `completed_at` sorting AFTER a higher-id one, which rules out completion ordering. The order was byte-identical across both samples while one run moved from in_progress to completed.

Id ordering also fixes where a run the page does not list yet BELONGS. A creation is therefore an insert rather than a flush. This is the case that mattered. The last deliveries of a CI run are a new job's creation and a queued job's start. They land in the seconds where GitHub's own listing is furthest behind them. Flushing there sent the next reader upstream at exactly that moment. The answer it absorbed was OLDER than the payloads already in hand. A finished run then sends nothing more, so the stale page stood for its whole TTL. One measured instance held an `all-builds` gate pending for a day with every job green. The cached page still reported a job that had finished as `in_progress`, and omitted a job created a second later.

Membership can therefore GROW from a payload, because a creation names the commit its run belongs to. What no delivery ever states is that a run went AWAY. That is what the TTL still bounds. The `workflow_jobs_cache` split has a fetch prove membership and deliveries maintain entries, with the one asymmetry that a creation is itself a membership statement.

The caller flushes when a rewrite is refused. A rewrite is refused for:

- A page other than the first.
- A page that does not hold every run, which is a `total_count` above its own length.
- An insert that spills past `per_page` onto a page this document does not hold.
- An insert into a page whose commit the run does not share.

A page carries no sha of its own. Every ENTRY does. That is what makes a page about another commit recognizable and left alone. It is also what makes an EMPTY page unplaceable, because it names no commit for a new run to match.

## Live job state

The Actions job reads were the largest passthrough in the mirror by a wide margin. `runs/{id}/jobs` alone was 67.5% of it. They were never a missing cache route. They were modeled and declined on purpose. Only a page whose every job had finished was stored, because a queued or running job is a live value the GHA runner coordinator provisions against. No TTL short enough to be safe there is long enough to be worth having.

That reasoning is right about a TTL and wrong about the only instrument available. A `workflow_job` delivery carries the whole job object. A stored answer therefore does not have to age toward the truth. It can be told. The split:

- A **fetch** proves the page's MEMBERSHIP, meaning which jobs belong to the run and in what order. Webhooks cannot establish that. Nothing in a delivery says "and that is all of them".
- The **deliveries** keep each entry's contents current, rewritten in place as the job moves.

Neither half works alone, which is why this is not "cache live state and hope".

The TTL is then the lost-delivery bound, which is the mirror's quietest failure. It is **three-way**, because one part of the answer no delivery can keep current. GitHub sends one delivery per job TRANSITION, meaning queued, in_progress and completed. A running job's `steps` advance between them unreported. So (`ghdata.JobsLiveness`):

| what is moving | clock | why |
| --- | --- | --- |
| nothing | 24h | only a re-run can change it |
| jobs WAITING to start | 60s | a queued job's steps are empty by construction, so a rewritten entry is exactly right |
| a job RUNNING | 10s | its steps drift with nothing to report them |

Read that table the other way. The one field webhooks cannot maintain is the one the shortest clock exists for. Pretending otherwise, by serving a running job's frozen step list for a minute, is the silent degradation this repo has a rule against.

What a delivery cannot answer drops the run's rows, so a fetch can settle them:

- A job the stored page does not list, which means the run's membership moved.
- A delivery whose `run_attempt` differs from the stored entry's, because a re-run is a different set of jobs whether or not it reuses ids.

## Single source of truth (the compare rework)

`compare_cache` exists because a three-dot compare is expensive to fetch. It must not exist at all. The mirror already stores commits in `git_commits_cache`, fed by every push and by fetch absorbs. A compare between two commits it holds is computable: walk to the merge base, then diff the trees. The happy path then needs no upstream call and no invalidation. The answer also stops being a second, separately-staleable copy of state the mirror already has.

That is the largest owed item in this file, and it is not a cache change. It needs tree and blob storage, which the mirror does not have today. The mirror stores commit objects, never the trees they point at.

## Owed, in order

1. `commits_list_cache` on push — needs the absent-vs-unknown modelling first.
2. `git_ref_cache` on a `delete` — needs the not-found ref document modelled, so a written verdict matches a fetched one.
3. The compare rework — needs tree storage, and retires an entry above.

Already done:

- The status documents on a `status` delivery.
- The check-runs listing on a `check_run` delivery.
- A run's job answers on a `workflow_job` delivery, which also made a RUNNING run cacheable.
- A sha's runs pages on a `workflow_run` delivery.
- The cross-kind flushing that had every CI delivery dropping every commit-CI kind.

Each rewrite rests on a measurement of the listing's ORDER, because a rewritten document has to be byte-identical to the fetch it saved. Those measurements are recorded above rather than assumed. The pattern they share is worth naming. Where an entry is APPEND-ONLY upstream, as a commit status is, the rewrite has to place it. Where an entry is UPDATED IN PLACE, as a check run, a workflow run and a job are, it only has to prove the entry does not move.
