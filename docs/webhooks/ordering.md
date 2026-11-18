# Out-of-order deliveries

GitHub does not order webhook deliveries and never claims to. Each is sent as it is produced, over its own connection, through a tunnel, into a server that may be restarting. So views of the same resource can arrive in either order. Applying the older one second is not a bug in the payload. It is a correct, repeatable write of superseded state, which is why idempotence does not help.

That failure is the quietest one this service has. A merged PR came back OPEN many seconds after its merge from a redelivered pre-merge payload and stayed that way for many minutes.

## The clock

There is no timestamp header to lean on. `Date` in particular moves with a retry rather than with the event.

So the clock comes from the payload, which is also the only part the signature covers. Rules decide whether a field can serve as one (`webhook.OrderOf`):

- **GitHub must set it, not a user.** A push's `head_commit.timestamp` is the COMMIT date — whatever the committer's machine claimed, unchanged by a rebase, and freely in the future. `repository.pushed_at` is GitHub's own record of the push. (Push payloads send it as a Unix integer while every other event sends RFC3339. Both are read.)
- **It must describe the subject, not the repo around it.** `updated_at` on a `pull_request` is the PR's. `repository.updated_at` riding along in the same payload is not.

A payload with no such field is UNORDERABLE and says so. It applies unconditionally — refusing state over a clock we never had will be strictly worse. We is counted separately so the share of the stream that is unordered is visible rather than assumed.

## The subject

The status row is the one that matters most and is easiest to get incorrect. One commit carries many contexts, and they supersede only themselves. Keyed on the sha alone, an `all-builds` result landing first will discard a `ci` result posted a second earlier. Silently losing a check this mirror is the source of truth for. `TestOrdering_StatusContextsOnOneShaDoNotSupersedeEachOther` pins it.

The `pull_request_review` row keys on the REVIEW, not the PR: two reviews on one PR are independent. Only a re-delivery of the same review is ordered against this one.

## The watermark

Every delivery is applied the moment it arrives. Nothing is held. Ordering is the watermark's job. The newest applied view of each subject is recorded, and a delivery older than it is refused with the `superseded` disposition rather than written. Equal times apply: GitHub's clocks are second-granular, and refusing on equality will drop a genuinely distinct view on rounding alone. The advance is a conditional upsert, so concurrent deliveries for one subject are resolved by the payload clock rather than by which transaction commits first.

Refusing is not a lesser outcome for a snapshot. GitHub payloads are full views, not deltas, so applying only the newest lands on the same last state ordering them will have.

### The exception: facts the newer view never restates

A push carries up to 2,048 commit objects, and a later push to the same branch does not repeat the earlier one's commits. Those are immutable and content-addressed, so absorbing them out of order is not a stale write at all. Dropping them will just mean fetching them back later. A superseded push therefore still absorbs its commits. The dashboard says so per refused delivery (`still_absorbed`). Everything else refused is a whole-resource snapshot, where the newer view states everything the older one did.

## What is reported

The Webhooks tab carries a Delivery ordering panel: the in-order count and the refusals. Refusals split into within-grace (≤10s, ordinary delivery jitter) and beyond-grace (something held the delivery — a redelivery, a start drain), mean and worst lateness, a per-event breakdown, and the last refusals in full. Both clocks, both delivery ids, which payload field the clock came from, and whether immutable facts were still taken.

## What this does not cover

The gate only sees DELIVERIES. A row written by a fetch absorb — the single-PR route, a list page, the consistency reconcile — passes no watermark. As a result, write-side guards remain load-bearing and independent: `pr_closures` refuses an open-PR view that cannot prove it postdates a recorded close. `NullPRMergeableOnTipMove` refuses retained pre-move merge fields. Guards, one shared failure. `TestDispatch_ClosureRecordStillRefusesAPreCloseViewWithoutTheGate` pins that the second one is not made redundant by the first.

Age (`ghda bounds watermarksta.WatermarkRetention`, several days) and live in the disposable cache DB. Losing them costs at most a window in which an out-of-order delivery is applied as it was before any of this existed.
