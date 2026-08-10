# Delivery gaps: the failure with no symptom

Every cache in this service is kept honest by webhooks. A push moves a branch
tip, the delivery applies it, and the answers computed against the old tip stop
being served. That is the whole model, and it has one assumption: the delivery
arrives.

Miss one and nothing says so. The mirror keeps serving what it last absorbed,
for the full TTL, and the consumer reading it cannot tell the difference
between "this is current" and "this is what was true nine hours ago". There is
no error, no log line, no red anything. It is the quietest failure the service
has.

**GitHub does not retry.** A delivery it could not hand over is not re-sent.
It is recorded in the App's own delivery log as a failure and left there.

## What it looks like from the outside

Observed live on 2026-08-10, two repos, two lost pushes:

| repo | master moved | mirror's `git/ref/heads/master` | age of the lie |
|---|---|---|---|
| `grok-build` | 10:01:29Z (`3de7d7c`) | `d753927`, served on a cache HIT | 21 min and counting |
| `webhooks` | 00:45:25Z (`a1d111c`) | `5db5993`, served on a cache HIT | 9.6 h |

A stale HIT is proof on its own: a reader would have fetched the current tip,
so the row can only be stale if it was written before the push and nothing
dropped it after. Deliveries themselves were fine — a branch push to each of
two repos was applied by the same running process within 15 seconds, measured
the same afternoon. These specific ones were simply lost.

## Why it stops a PR forever

The worst shape of this is an answer that **stops its own repair**.

pr-minder asks the mirror `GET /compare/master...<head>` to decide whether a
PR needs its branch updated. With the push lost, the cached comparison still
says `behind_by: 0`, so pr-minder concludes there is nothing to do. It does not
update the branch. Nothing else re-asks. The PR sits with auto-merge armed and
`mergeStateStatus: BEHIND` until a human notices.

Compare that to an ordinary stale read, which the next reader corrects. Here
the stale read is exactly what prevents the next read from ever happening.

## What the mirror does about it

Two independent mechanisms, because they fail differently.

### 1. Recover the delivery (`internal/sync/replay.go`)

GitHub keeps the log and will re-send on request, and the mirror holds the App
credential that can read it:

- `GET /app/hook/deliveries?status=failure` — every delivery GitHub could not
  hand over, newest first. JWT auth, app-level.
- `POST /app/hook/deliveries/{id}/attempts` — send it again. The replay arrives
  as an ordinary delivery through the ordinary handler.

The replayer runs one cycle at startup (a restart is itself a window in which
deliveries fail, so a fresh process asks what it missed straight away) and then
one per `WEBHOOK_REPLAY_INTERVAL` (default 5m).

Bounds, and why each exists:

- **Asked once per delivery id**, remembered in `webhook_replays`. GitHub
  leaves a failed delivery listed indefinitely, so without this one lost push
  becomes a replay request every cycle, forever. The row is written *before*
  the request: a request whose outcome we did not see may well have been
  accepted, and a repeat is the worse failure. A redelivery gets its own
  delivery id, so a replay that also fails is separately requestable.
- **`ReplayLookback` (24h).** Past it, the caches that delivery would have
  moved have expired anyway, so a replay applies state nothing is serving
  stale.
- **`ReplayPerCycle` (25).** A restart during a busy minute fails deliveries in
  bulk; the cap keeps recovery from arriving as a flood of its own. What is
  skipped is logged at error level with the count, and the next cycle takes it.
- Handlers are idempotent (upserts and applies), so a replayed event that *was*
  processed the first time is harmless.

Not covered, stated plainly: a delivery GitHub records as **successful** that
the mirror then failed to act on. That is a dispatcher disposition, visible in
the delivery log, and a different problem.

#### A replay is an old view, and old views can overwrite new ones

Idempotence is not the property that makes a replay safe, and treating it as
one produced the mirror's next bug. GitHub re-sends the payload it built *when
the event happened*. Applying it later is a correct, repeatable write — of
state that has since been superseded.

Measured on 2026-08-10: `agentic-loop#24` merged at 15:08:16Z. At 15:09:38Z —
82 seconds later, on the replayer's cycle — the PR was written back into the
cache as **open**, from a payload stamped `updated_at: 15:06:10Z`. Only open
PRs are retained, so the close had *deleted* the row, and a deleted row has
nothing to lose a comparison against: the stale view simply re-inserted it.
Nothing restates a close. It was still open 44 minutes later, when the
consistency check found it.

The fix is at the **write**, not here. An ordinary late delivery does exactly
the same damage — GitHub delivers concurrently, so a slow `synchronize` can
land after the `closed` it preceded — and nothing the replayer could check
would catch that one. So a close now leaves a record (`pr_closures`: the PR,
and the `updated_at` of the view that reported the close), and every open-PR
write path refuses a view that cannot prove it postdates it. A genuine reopen
carries a later `updated_at`, applies, and clears the record on the way
through. The record expires at `ghdata.PRClosureRetention` (24h — the replay
lookback, since nothing else re-sends a delivery at all).

A write with **no** `updated_at` is refused too: it cannot prove anything, and
refusing what cannot prove it is the entire job. Every real source stamps it.

Only a **statement** that the PR closed records one — the `closed` delivery,
or a single-PR fetch answering non-open. The reconcile sweeps (the org
snapshot, the complete `/pulls` page) delete rows by inferring the close from
*absence* from an eventually-consistent list, which is exactly why those paths
carry a grace window. A closure recorded from a wrong inference would refuse
the PR's real deliveries for a day; a wrong delete costs one refetch, and the
sweep re-runs.

### 2. Notice the staleness on read (`compare_cache.base_tip_sha`)

Recovery narrows the window; it does not close it. So the one answer whose
staleness is self-sustaining also carries its own proof.

GitHub states `base_commit.sha` on every comparison: the exact base-side tip
the answer was computed against. The mirror records it on the row. On read,
`ghdata.baseBranchMoved` compares it against what `git_ref_cache` says the
branch is on now — which a push keeps current by applying its own `after`
(`ApplyPushedRefTip`). A mismatch is a miss, whatever happened to the flush.

Deliberately narrow:

- Only the **base** side. GitHub states no head tip, and deriving one from the
  last element of `commits` is wrong the moment a comparison exceeds GitHub's
  250-commit cap — a truncated list whose tail is not the head.
- A **sha** on the base side is immutable; nothing to check.
- A branch whose tip has **never been observed** falls through to the existing
  contract (flush, then TTL). Refusing there would not make any answer fresher,
  it would just turn every first comparison into a permanent miss.
- **Tags are out of scope.** An annotated tag's ref object is the tag object,
  not the commit, so its sha would never equal a base tip and every tag-based
  comparison would read as moved forever.

## Operating it

- `WEBHOOK_REPLAY_INTERVAL` — cycle length, Go duration, default `5m`. `0`
  disables recovery entirely; the server warns at startup when it is off, and
  when no App is configured (the failure log needs the App's JWT).
- Every replay request logs at WARN with the delivery id, guid, event, and
  GitHub's own failure status. A quiet replayer means no gaps, not no checking.
- Hitting the per-cycle cap logs at ERROR with how many are left.
