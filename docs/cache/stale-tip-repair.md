# Keeping a branch tip right, without anyone asking

`GET /repos/{owner}/{repo}/git/ref/{ref}` answers with one mutable pointer,
which makes it the cached route where a lost delivery hurts most. Every other
cached answer that lags is merely old; this one is the input to decisions about
whether there is any work to do, so a consumer reading it wrong concludes there
is nothing to do and never asks again. The answer stops its own repair.

Correctness here is THIS SERVICE's problem. A consumer cannot notice a delivery
that never arrived, and asking one to send fixup requests inverts the
responsibility the mirror exists to hold.

## What happened (xml-validator#10)

`wow-look-at-my/xml-validator` merged PR #11 into `master` at 15:35:26 on
2026-08-14, moving the branch from `2fa82e7a` to `d197a949`. The stored answer
for `heads/master` never moved: an hour later it was still serving `2fa82e7a`
as a cache HIT, under a 24h TTL, with nothing else able to correct it — pushes
APPLY their own tip, so a delivered push would have, and nothing else writes
the row.

pr-minder read that tip and computed the PR's diff against it:

| base tip used              | ahead | behind | changed files | verdict     |
| -------------------------- | ----- | ------ | ------------- | ----------- |
| `2fa82e7a` (mirror, stale) | 4     | 0      | 10            | `has-diff`  |
| `d197a949` (real)          | 3     | 0      | 0             | `zero-diff` |

The PR was provably content-empty against its real base — its tree is
byte-identical to `master`'s — but read as an ordinary PR with ten changed
files, so its zero-diff auto-merge never fired and the hourly reconcile
recomputed the same wrong answer every hour.

Note what the mirror held at that moment: `git_ref_cache["heads/master"]` said
`2fa82e7a` while `pull_requests[#10].base_ref_oid` said `d197a949`. Two rows,
same branch, same database, disagreeing — and nothing reconciled them.

## The two mechanisms

**1. Every delivery that STATES the tip applies it.** A merge arrives twice —
once as a `push` for the base branch, once as `pull_request` with
`merged: true` — and the second one names the commit merging put on the base
branch (`merge_commit_sha`). `ApplyMergedBaseTip` writes it into the cached ref
rows, so both deliveries have to be lost before the tip goes wrong. Ordering is
decided without a fetch: the row records when its content was established, and
a merge that predates that instant says nothing the row does not already
account for, so only a merge that postdates it applies. An open PR's
`merge_commit_sha` is the throwaway test-merge commit, which is on no branch at
all, and is never read as a tip.

**2. A row another row contradicts is not served.** `GetCachedGitRefChecked`
asks, before replaying a stored tip, whether any absorbed OPEN PR based on that
branch names a different `base.sha`. If one does, the mirror does not pick a
side — it refetches and serves GitHub's answer. Two conditions keep that from
firing forever, because a PR's `base.sha` legitimately LAGS its branch (GitHub
recomputes it on PR contact, not on every base push), so most disagreements are
lag rather than staleness:

- the contradicting view must have been absorbed AFTER the row was fetched
  (`touched_at > fetched_at`) — evidence older than our own answer was already
  accounted for by fetching it;
- the sha must not be the one a previous fetch already settled
  (`reconciled_against`) — re-absorbing the same lagging value is not new
  evidence.

That bounds it to one refetch per genuinely new disagreeing sha per branch,
roughly one per base-branch move, and it is deliberately NOT an ancestry test:
proving which sha is newer costs the same upstream call as simply fetching the
truth.

## What was deliberately not done

**A client-triggered revalidation** (`Cache-Control: no-cache` on the route,
sent by a consumer that had detected the contradiction). It works, and it was
the first cut of this fix, and it is the wrong shape: it makes the mirror's
correctness depend on every consumer noticing, implementing, and being trusted
to ask. The mirror holds both contradicting facts itself and can act on them
without being told.

**Shortening the TTL.** It hides the window instead of closing it (CLAUDE.md:
"Never answer a gap with a shorter TTL"), and it would spend a refetch per
branch per interval on every repo to paper over a case that is rare and now
detected.

**Cross-checking the row against the mirror's other CACHES.** The branches
listing, the compare rows and `KnownBranchTip` are all fed by the same delivery
stream, so the same lost delivery poisons them identically — `KnownBranchTip`
reads these very rows, which is why the compare route's own staleness refusal
could not catch this: it was anchored on the wrong row. The PR rows work as
evidence precisely because they arrive on a DIFFERENT delivery and a different
route.

## What is still uncovered

A branch with no open PR based on it, whose push delivery is lost, stays stale
until the TTL — mechanism 2 has no second row to check it against. The
delivery replayer (docs/webhooks/delivery-gaps.md) is what bounds that today.
