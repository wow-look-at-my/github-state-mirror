# Repairing a branch tip the mirror got wrong

`GET /repos/{owner}/{repo}/git/ref/{ref}` answers with one mutable pointer, and
that makes it the cached route where a lost delivery hurts most. Every other
cached answer that lags is merely old; this one is the input to decisions about
whether there is any work to do, so a consumer reading it wrong concludes there
is nothing to do and never asks again. The answer stops its own repair.

## What happened (xml-validator#10)

`wow-look-at-my/xml-validator` merged PR #11 into `master` at 15:35:26 on
2026-08-14, moving the branch from `2fa82e7a` to `d197a949`. The mirror's stored
answer for `heads/master` never moved: an hour later it was still serving
`2fa82e7a` as a cache HIT, under a 24h TTL with nothing else able to correct it
(pushes APPLY their own tip, so a delivered push would have; nothing else
writes the row).

pr-minder read that tip and computed the PR's diff against it:

| base tip used            | ahead | behind | changed files | verdict     |
| ------------------------ | ----- | ------ | ------------- | ----------- |
| `2fa82e7a` (mirror, stale) | 4     | 0      | 10            | `has-diff`  |
| `d197a949` (real)          | 3     | 0      | 0             | `zero-diff` |

The PR was provably content-empty against its real base -- its tree is
byte-identical to `master`'s -- but read as an ordinary PR with ten changed
files. pr-minder's zero-diff force-merge never fired, and its hourly reconcile
recomputed the same wrong answer from the same stale row every hour. Nothing in
either system could notice: the consumer had no second opinion, and the mirror
had no reason to doubt a row inside its TTL.

## The two repairs

**1. A second delivery states the same fact.** A merge arrives twice -- once as
a `push` for the base branch, once as `pull_request` with `merged: true` -- and
the second one names the commit merging put on the base branch
(`merge_commit_sha`). `ApplyMergedBaseTip` writes it into the cached ref rows,
so both deliveries have to be lost before the tip goes wrong. Ordering is
decided without a fetch: the row records when its content was established, and
a merge that predates that instant says nothing the row does not already
account for, so only a merge that postdates it applies. An open PR's
`merge_commit_sha` is the throwaway test-merge commit, which is on no branch at
all, and is never read as a tip.

**2. A consumer holding proof can repair the row.** `Cache-Control: no-cache`
on the ref route skips the stored answer, refetches, rewrites the row and
answers `X-GSM-Cache: revalidated`. It is not a cache opt-out: the refetch
spends the CALLER's rate-limit budget, and the fresh answer is stored, so one
caller's revalidation fixes the row for every other reader. The response header
is what makes it honest -- a caller that asked for a refetch and reads `hit`
back knows this mirror does not implement one and must say so rather than
assume it was served fresh bytes.

pr-minder is the consumer that holds the proof: it cross-checks the tip against
GitHub's own `base.sha` for the PR in front of it, proves by ancestry which one
is newer, and revalidates here when the stored one is behind. See the webhooks
repo's `docs/hooks/pr-minder/base-tip-resolution.md`.

## What was deliberately not done

**Shortening the TTL.** It hides the window instead of closing it (CLAUDE.md:
"Never answer a gap with a shorter TTL"), and it would spend a refetch per
branch per interval on every repo to paper over a case that is rare and now
detectable.

**Cross-checking the row against other cached state.** Everything else the
mirror holds about a branch tip -- the branches listing, the compare rows,
`KnownBranchTip` itself -- is fed by the same delivery stream, so the same lost
delivery poisons all of them identically. `KnownBranchTip` in particular reads
these very rows, which is why the compare route's own staleness refusal could
not catch this: it was anchored on the wrong row. Only an independent source
(another delivery, or the consumer's own view of GitHub) can break the tie.
