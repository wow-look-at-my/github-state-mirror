# Every invalidation, and whether it is defensible

CLAUDE.md's rule: **apply the payload; invalidation is the last resort.** A
delivery is the answer, not a cache-busting ping — dropping a row and buying
the same fact back over HTTP is a bug report, not a design.

This file is the complete inventory of the places this service still drops a
row instead of writing one, each with what the payload actually carries, and a
verdict. It exists so the count can only go down: an entry here is either
justified with a reason that survives reading, or it is named as owed work.

Two things are deliberately NOT in this file. `docs/webhooks/payload-unused/`
covers whole HANDLERS that consume no payload at all, and is a build gate
(`TestWebhookHandlersConsumeTheirPayload`). This file covers the response-cache
flushes in `internal/sync/webhook_invalidate.go`, which are disposition-neutral
bookkeeping and which no test can classify for you.

## Legend

- **Justified** — the payload cannot answer, and the reason is specific to this
  row rather than a general shrug.
- **Owed** — the payload could answer and does not yet. Named, with the
  conversion.

## push (`invalidateForPush`)

| row family | verdict | why |
| --- | --- | --- |
| `git_ref_cache` | **applied** | the push STATES the ref's new tip (`after`); `ApplyPushedRefTip` writes it. The delete is the fallback for a deletion (all-zeros tip), a 404-verdict row a creation must clear, and an unreadable row. |
| `branches_list_cache` | **applied** | a tip move changes one entry's sha, which the payload states; the pages are rewritten in place. Only create/delete, which move page MEMBERSHIP, fall back to a flush. |
| `contents_cache` | **Justified** | the payload names the files each commit added/modified/removed. It never carries their CONTENT, and the cached answer IS the content (plus its blob sha, size and encoding). Nothing to write. |
| `commits_list_cache` | **Owed** | the payload carries up to 2,048 full commit objects, oldest-first, and the cached answer is exactly "commits from this ref, newest first". Page 1 of an unfiltered listing could be rebuilt by prepending the pushed commits. Blocked on: the rendered doc carries per-commit fields the push payload does not (author/committer `login`, `html_url`-free identity, verification), so a prepended entry would not be byte-identical to a fetched one — which the hit/miss identity rule forbids. Conversion needs those fields modelled as absent-vs-unknown first. |
| `compare_cache` | **Justified today, superseded later** | a diff is content, and no push payload carries it. The real answer is not to apply this row but to stop storing it: with the commits cached, a three-dot compare is computable locally (see "Single source of truth" below), at which point this cache and its invalidation both disappear. |
| `commit_ci_cache` (branch-form rows) | **Justified** | the rows keyed by a branch name describe "the CI of whatever that branch points at". The push moves the branch, and the payload says nothing about the new tip's checks — there are usually none yet, but a force-push can point a branch at an old commit that has a full set. Writing "no checks" would be a guess that is wrong exactly when it matters. Sha-form rows are untouched: they describe an immutable commit. |
| `pull_files_cache`, `pull_diff406_cache` | **Justified** | a base push moves every open PR's merge-base-relative file list, and the payload carries no per-PR signal at all — not even which PRs exist. The repo-wide flush is the belt for the per-PR flush that `pull_request` deliveries do carry. |
| `pull_commits` snapshots | **Justified** | same shape: a fork head's pushes never reach us, so this is the repo-wide belt for a missed `pull_request` delivery. |
| PR `mergeable` fields | **applied-as-unresolved** | GitHub recomputes mergeability asynchronously and no webhook ever carries the recomputed value, so the stored answer is un-resolved rather than dropped, with the invalidated test-merge sha remembered so a refetch re-offering it is recognized as pre-push. |

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
| PR `mergeable_state` | **applied-as-unresolved** | a CI result is the only thing that moves unstable/blocked ↔ clean, and no payload carries the recomputed value. |
| `commit_ci_cache`, the `/status` and `/statuses` docs, on a `status` event | **applied** | the payload IS the status, whole. `SettleCommitCIFromStatus` rewrites both documents from it — see "The status rewrite" below for the ordering rules that make the result exact rather than plausible — and drops only what a stored page cannot provably hold. |
| `commit_ci_cache`, the `/check-runs` docs, on a check event | **Owed** | a `check_run` delivery carries the whole run object, which is what the listing holds. The same conversion applies and has not been written; unlike the statuses array, the listing's ordering has not been measured, and a guess there produces a document a fetch would not have returned. |
| the OTHER kinds, on either event | **applied** (nothing to do) | a commit status never appears in a check-runs listing and a check run never appears in a commit's statuses. Flushing all three kinds on every CI delivery re-fetched answers the delivery could not have changed; each now flushes only its own surface. |
| `workflow_runs_cache` (sha pages) | **Owed** | a `workflow_run` delivery carries the whole run object, which is what these pages list. Same shape of conversion, same missing measurement. |

## workflow_job / workflow_run

| row family | verdict | why |
| --- | --- | --- |
| `workflow_jobs` truth | **applied** | job state is written from the payload. |
| `workflow_jobs_cache` (run/job pages) | **applied** | the delivery carries the whole job object — labels, runner, every step — which is exactly one entry of these answers, so `ApplyWorkflowJob` rewrites it in place. See "Live job state" below: this is also what made a RUNNING run cacheable, which was three quarters of the mirror's passthrough volume. The flush stays for what a delivery cannot answer: a job the page does not list (the run's membership moved) and a different `run_attempt` (a re-run is a different set of jobs). |

## repository

Everything repo-wide. **Justified**: a rename, delete or visibility change makes
the rows WRONG rather than stale — they are keyed by a name that no longer means
what it did, or hold data a now-private repo must not serve. The payload states
the repo's new identity, not the new content of every answer keyed by the old
one.

## installation / installation_repositories

`install_token_cache` and `repo_installation_cache` by id, plus a sweep of every
absent-installation verdict and the whole `installation_repos_cache`.
**Justified**: the delivery is precisely the news that an installation's
coverage changed, and neither the verdicts nor the listings carry an id to match
on. A suspended installation must stop serving minted tokens immediately;
applying anything here would mean inventing the new coverage.

## label

Repo-wide on every action. **Justified**: an `edited` action can RENAME a label
(two names in one delivery) and one label answers under every spelling a caller
might request, so matching the payload's name would miss rows. The grain is the
repo because the key space is unbounded, not because the payload is thin.

## The status rewrite

Rewriting a stored document from a payload is only correct if the result is
what a fetch would have returned, byte for byte — a hit replays the stored
bytes. For the two status-shaped documents that came down to their ORDERING,
which the REST docs do not state. It was measured instead
(`kubernetes/kubernetes` @`a231bf3f`, 53 raw statuses over 22 contexts):

- Statuses are **append-only**. Re-posting a context creates a new status
  object with a new id and a new `created_at`; nothing is mutated in place.
  That is why the raw list keeps all 53 and the combined status keeps 22.
- `/statuses` is **newest-first** — a new status is PREPENDED, and the
  context's older entries stay.
- `/status` holds the **latest per context, oldest-first** (exactly the raw
  list's per-context latest, reversed), so a new status leaves its context's
  old position and lands at the END.
- Its `state` is the documented rollup: failure if any context is error or
  failure, pending if there are none or any is pending, success otherwise.
  `total_count` is the array's own length.

Every rewrite refuses unless the stored page PROVES it can hold the result:
page 1, the whole set present, room for a new entry, and a status that is the
newest on the commit (an older one's position is unknown). A refusal drops the
row, which is what happened to every one of these before the rewrite existed.

Which rows are reached is decided by what the payload can IDENTIFY, not by the
ref spellings the delivery names: a combined status states the sha it resolved
to, so a branch-form row is usable and a row about another commit is left
alone. The raw list is a bare array with no sha in it, so only the sha-form row
is provably about this commit and the branch spellings still flush.

`TestCachedCommitCI_StatusEventRewritesTheCombinedDoc` (internal/api) is the
pin: it byte-compares a rewritten document against the fetch it saved.

## Live job state

The Actions job reads were the largest passthrough in the mirror by a wide
margin — `runs/{id}/jobs` alone was 67.5% — and they were never a missing cache
route. They were modeled and declined on purpose: only a page whose every job
had finished was stored, because a queued or running job is a live value the
GHA runner coordinator provisions against, and no TTL short enough to be safe
there is long enough to be worth having.

That reasoning is right about a TTL and wrong about the only instrument
available. A `workflow_job` delivery carries the whole job object, so a stored
answer does not have to age toward the truth — it can be told. The split:

- A **fetch** proves the page's MEMBERSHIP: which jobs belong to the run, in
  what order. Webhooks cannot establish that; nothing in a delivery says "and
  that is all of them".
- The **deliveries** keep each entry's contents current, rewritten in place as
  the job moves.

Neither half works alone, which is why this is not "cache live state and hope".

The TTL is then the lost-delivery bound — the mirror's quietest failure — and
it is **three-way**, because one part of the answer no delivery can keep
current. GitHub sends one delivery per job TRANSITION (queued, in_progress,
completed), while a running job's `steps` advance between them unreported. So
(`ghdata.JobsLiveness`):

| what is moving | clock | why |
| --- | --- | --- |
| nothing | 24h | only a re-run can change it |
| jobs WAITING to start | 60s | a queued job's steps are empty by construction, so a rewritten entry is exactly right |
| a job RUNNING | 10s | its steps drift with nothing to report them |

Reading that table the other way: the one field webhooks cannot maintain is the
one the shortest clock exists for. Pretending otherwise — serving a running
job's frozen step list for a minute — is the silent degradation this repo has a
rule against.

Two things a delivery cannot answer, both of which drop the run's rows so a
fetch can settle them: a job the stored page does not list (the run's
membership moved), and a delivery whose `run_attempt` differs from the stored
entry's (a re-run is a different set of jobs, whether or not it reuses ids).

## Single source of truth (the compare rework)

`compare_cache` exists because a three-dot compare is expensive to fetch. It
should not exist at all: the mirror already stores commits (`git_commits_cache`,
fed by every push and by fetch absorbs), and a compare between two commits it
holds is computable — walk to the merge base, diff the trees. The happy path
then needs no upstream call and no invalidation, and the answer stops being a
second, separately-staleable copy of state the mirror already has.

That is the largest owed item in this file and it is not a cache change: it
needs tree/blob storage, which the mirror does not have today (it stores commit
objects, not the trees they point at).

## Owed, in order

1. `commit_ci_cache`'s check-runs documents on a `check_run` delivery — the
   same rewrite the status half now does. Needs the listing's ordering
   measured first, the way the statuses arrays were.
2. `workflow_runs_cache` on workflow_run — same shape.
3. `commits_list_cache` on push — needs the absent-vs-unknown modelling first.
4. The compare rework — needs tree storage, and retires two entries above.

Done: `commit_ci_cache`'s two status documents on a `status` delivery, and the
cross-kind flushing that had every CI delivery dropping all three.
