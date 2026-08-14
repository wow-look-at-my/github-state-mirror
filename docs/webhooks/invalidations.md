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
| `commit_ci_cache` (the rendered `/status`, `/check-runs` docs) | **Owed** | this is the clearest remaining violation. The payload carries the full status/check-run object — id, context, state, description, timestamps — and the mirror already applies it to `commit_checks`; then it DROPS the rendered doc, so the next reader buys the same answer back from GitHub. The truth table stores only `(sha, context, state)`, which is why a rebuild from truth alone would be lossy — but the stored DOC can be patched in place from the payload, exactly as `ApplyPushedRefTip` patches a ref doc: replace the entry for this context, recompute the rollup, keep every other entry. Conversion: a `ApplyCommitCIFromPayload` on the sha-form rows, with the branch-form rows still flushed (they may point elsewhere). |
| `workflow_runs_cache` (sha pages) | **Owed** | a `workflow_run` delivery carries the whole run object, which is what these pages list. Same shape of conversion as above, and the same reason it has not happened yet: the rendered page holds run fields the truth table does not. |

## workflow_job / workflow_run

| row family | verdict | why |
| --- | --- | --- |
| `workflow_jobs` truth | **applied** | job state is written from the payload. |
| `workflow_jobs_cache` (run/job pages) | **Justified** | only TERMINAL answers are ever stored, and a re-run replaces a run's jobs under the same run id. The flush is what makes a re-run's new jobs visible; there is no "the payload states the whole page" to apply, because a page is many jobs and a delivery is one. |

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

1. `commit_ci_cache` on status/check events — patch the stored doc from the
   payload instead of dropping it. Highest volume, smallest change.
2. `workflow_runs_cache` on workflow_run — same shape.
3. `commits_list_cache` on push — needs the absent-vs-unknown modelling first.
4. The compare rework — needs tree storage, and retires two entries above.
