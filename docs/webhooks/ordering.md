# Out-of-order deliveries

GitHub does not order webhook deliveries and never claims to. Each one is sent
as it is produced, over its own connection, through a tunnel, into a server
that may be restarting — so two views of the same resource can arrive in
either order. Applying the older one second is not a bug in the payload: it is
a correct, repeatable write of superseded state, which is why idempotence does
not help.

That failure is the quietest one this service has. A merged PR came back OPEN
82 seconds after its merge from a redelivered pre-merge payload and stayed that
way for 44 minutes.

## The clock

There is no timestamp header to lean on. GitHub sends `X-GitHub-Delivery` (a
UUID, not time-ordered), `X-GitHub-Event`, `X-GitHub-Hook-ID` and the
signature. Everything else on the wire here — `CF-Ray`, `CF-Connecting-IP`,
`X-Forwarded-*`, `Date` — is added by Cloudflare's tunnel and describes our
hop, not GitHub's; `Date` in particular moves with a retry rather than with the
event.

So the clock comes from the payload, which is also the only part the signature
covers. Two rules decide whether a field can serve as one (`webhook.OrderOf`):

- **GitHub must set it, not a user.** A push's `head_commit.timestamp` is the
  COMMIT date — whatever the committer's machine claimed, unchanged by a
  rebase, and freely in the future. `repository.pushed_at` is GitHub's own
  record of the push. (Push payloads send it as a Unix integer while every
  other event sends RFC3339; both are read.)
- **It must describe the subject, not the repo around it.** `updated_at` on a
  `pull_request` is the PR's. `repository.updated_at` riding along in the same
  payload is not.

A payload with no such field is UNORDERABLE and says so. It applies
unconditionally — refusing state over a clock we never had would be strictly
worse — and is counted separately so the share of the stream that is
unordered is visible rather than assumed.

## The subject

Ordering is per RESOURCE, at the grain where "older" actually means
"superseded":

| event | subject | clock |
| --- | --- | --- |
| push | `ref:owner/repo:refs/heads/x` | `repository.pushed_at` |
| pull_request | `pr:owner/repo#12` | `pull_request.updated_at` |
| pull_request_review | `review:owner/repo:<id>` | `review.submitted_at` |
| issue_comment | `comment:owner/repo:<id>` | `comment.updated_at` |
| status | `status:owner/repo:<sha>:<context>` | `updated_at` |
| check_run | `check_run:owner/repo:<id>` | `completed_at` ?? `started_at` |
| check_suite | `check_suite:owner/repo:<id>` | `check_suite.updated_at` |
| workflow_run | `workflow_run:owner/repo:<id>` | `workflow_run.updated_at` |
| workflow_job | `workflow_job:owner/repo:<id>` | `completed_at` ?? `started_at` |
| repository | `repo:owner/repo` | `repository.updated_at` |
| installation | `installation:<id>` | `installation.updated_at` |

The status row is the one that matters most and is easiest to get wrong. One
commit carries many contexts, and they supersede only themselves: keyed on the
sha alone, an `all-builds` result landing first would discard a `ci` result
posted a second earlier — silently losing a check this mirror is the source of
truth for. `TestOrdering_StatusContextsOnOneShaDoNotSupersedeEachOther` pins
it.

The `pull_request_review` row keys on the REVIEW, not the PR: two reviews on
one PR are independent, and only a re-delivery of the same review is ordered
against this one.

`label` (no timestamp anywhere in the payload), `organization`, and
`membership` are unorderable and apply as they always did — reported, never
guessed at.

## Two mechanisms, different distances

**1. The reorder window** (`internal/sync/reorder.go`, `WEBHOOK_REORDER_WINDOW`,
2s). The first delivery for a subject opens a window; later deliveries for that
same subject join it; when it closes the batch is sorted by the payload clock
and dispatched oldest-first. Inside the window nothing is refused — both views
apply, in the order they happened.

It has to be a WINDOW, not a delay. Holding every delivery for a fixed 2s
preserves arrival order exactly, so the case worth fixing — the older view
arriving second — comes out in the same wrong order it went in. Batching per
subject is also what keeps the cost off unrelated events: a busy repo's PR
deliveries never wait behind another repo's push.

The window is small on purpose and capped at 5s. Every delivery pays it in
latency, and GitHub's own delivery timeout is single-digit seconds — a hold
long enough to catch a redelivery would start failing deliveries, which is the
failure ordering exists to reduce.

**2. The watermark** (`internal/sync/ordering.go`,
`ghdata.ClaimEventOrder`, `webhook_watermarks`). Past the window, the newest
applied view of each subject is recorded, and a delivery older than it is
refused with the `superseded` disposition rather than written. Equal times
apply: GitHub's clocks are second-granular, and refusing on equality would drop
a genuinely distinct view on rounding alone. The advance is a conditional
upsert, so concurrent deliveries for one subject are resolved by the payload
clock rather than by which transaction commits first.

Refusing is not a lesser outcome for a snapshot. GitHub payloads are full
views, not deltas, so applying only the newest lands on the same final state
ordering them would have.

### The exception: facts the newer view never restates

A push carries up to 2,048 commit objects, and a later push to the same branch
does not repeat the earlier one's commits. Those are immutable and
content-addressed, so absorbing them out of order is not a stale write at all —
dropping them would just mean fetching them back later. A superseded push
therefore still absorbs its commits, and the dashboard says so per refused
delivery (`still_absorbed`). Everything else refused is a whole-resource
snapshot, where the newer view states everything the older one did.

## What is reported

The Webhooks tab carries a Delivery ordering panel: in-order count, batches the
window actually re-sorted, refusals split into within-grace (≤10s, ordinary
delivery jitter) and beyond-grace (something held the delivery — a redelivery,
a restart drain), mean and worst lateness, a per-event breakdown, and the last
50 refusals in full — both clocks, both delivery ids, which payload field the
clock came from, and whether immutable facts were still taken.

`held` with a zero `reordered` is the window costing latency and buying
nothing; that is the number to watch before changing `WEBHOOK_REORDER_WINDOW`.

## What this does not cover

The gate only sees DELIVERIES. A row written by a fetch absorb — the single-PR
route, a list page, the consistency reconcile — passes no watermark, so
write-side guards remain load-bearing and independent: `pr_closures` refuses an
open-PR view that cannot prove it postdates a recorded close, and
`NullPRMergeableOnTipMove` refuses retained pre-move merge fields. Two guards,
one shared failure;
`TestDispatch_ClosureRecordStillRefusesAPreCloseViewWithoutTheGate` pins that
the second one is not made redundant by the first.

Watermarks are bounded by age (`ghdata.WatermarkRetention`, 7 days) and live in
the disposable cache DB. Losing them costs at most a window in which an
out-of-order delivery is applied as it was before any of this existed.
