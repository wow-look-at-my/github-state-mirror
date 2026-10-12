# Out-of-order deliveries

GitHub does not order webhook deliveries and never claims to. Each is sent as it is produced, over its own connection, through a tunnel, into a server that may be restarting. So views of the same resource can arrive in either order. Applying the older one second is not a bug in the payload. It is a correct, repeatable write of superseded state, which is why idempotence does not help.

That failure is the quietest one this service has. A merged PR came back OPEN many seconds after its merge from a redelivered pre-merge payload and stayed that way for many minutes.

## The clock

There is no timestamp header to lean on. GitHub sends `X-GitHub-Delivery` (a UUID, not time-ordered), `X-GitHub-Event`, `X-GitHub-Hook-ID` and the signature. Cloudflare's tunnel and describes our hop ads everything else on the wire here — `CF-Ray`, `CF-Connecting-IP`, `X-Forwarded-*`, `Date` —, not GitHub's. `Date` in particular moves with a retry rather than with the event.

So the clock comes from the payload, which is also the only part the signature covers. Rules decide whether a field can serve as one (`webhook.OrderOf`):

- **GitHub must set it, not a user.** A push's `head_commit.timestamp` is the COMMIT date — whatever the committer's machine claimed, unchanged by a rebase, and freely in the future. `repository.pushed_at` is GitHub's own record of the push. (Push payloads send it as a Unix integer while every other event sends RFC3339. Both are read.)
- **It must describe the subject, not the repo around it.** `updated_at` on a `pull_request` is the PR's. `repository.updated_at` riding along in the same payload is not.

A payload with no such field is UNORDERABLE and says so. It applies unconditionally — refusing state over a clock we never had will be strictly worse. We is counted separately so the share of the stream that is unordered is visible rather than assumed.

## The subject

Ordering is per RESOURCE, at the grain where "older" means "superseded":

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

The status row is the one that matters most and is easiest to get incorrect. One commit carries many contexts, and they supersede only themselves. Keyed on the sha alone, an `all-builds` result landing first will discard a `ci` result posted a second earlier. Silently losing a check this mirror is the source of truth for. `TestOrdering_StatusContextsOnOneShaDoNotSupersedeEachOther` pins it.

The `pull_request_review` row keys on the REVIEW, not the PR: two reviews on one PR are independent. Only a re-delivery of the same review is ordered against this one.

`label` (no timestamp anywhere in the payload), `organization`, and `membership` are unorderable and apply as they always did — reported, not guessed at.

## The watermark

Every delivery is applied the moment the worker reaches it, and none is held for a clock. The worker applies the client lane ahead of the Actions bulk lane, and each lane in arrival order (docs/webhooks/lanes.md). No subject spans both lanes. Ordering is the watermark's job (`internal/sync/ordering.go`, `ghdata.ClaimEventOrder`, `webhook_watermarks`). The newest applied view of each subject is recorded, and a delivery older than it is refused with the `superseded` disposition rather than written. Equal times apply: GitHub's clocks are second-granular, and refusing on equality will drop a genuinely distinct view on rounding alone.

The advance is a conditional upsert, so concurrent deliveries for one subject are resolved by the payload clock rather than by which transaction commits first.

Refusing is not a lesser outcome for a snapshot. GitHub payloads are full views, not deltas, so applying only the newest lands on the same last state ordering them will have.

### The exception: facts the newer view never restates

A push carries up to 2,048 commit objects, and a later push to the same branch does not repeat the earlier one's commits. Those are immutable and content-addressed, so absorbing them out of order is not a stale write at all. Dropping them will just mean fetching them back later. A superseded push therefore still absorbs its commits. The dashboard says so per refused delivery (`still_absorbed`). Everything else refused is a whole-resource snapshot, where the newer view states everything the older one did.

## What is reported

The Webhooks tab carries a Delivery ordering panel: the in-order count and the refusals. Refusals split into within-grace (≤10s, ordinary delivery jitter) and beyond-grace (something held the delivery — a redelivery, a start drain), mean and worst lateness, a per-event breakdown, and the last refusals in full. Both clocks, both delivery ids, which payload field the clock came from, and whether immutable facts were still taken.

## What this does not cover

The gate only sees DELIVERIES. A row written by a fetch absorb — the single-PR route, a list page, the consistency reconcile — passes no watermark. As a result, write-side guards remain load-bearing and independent: `pr_closures` refuses an open-PR view that cannot prove it postdates a recorded close. `NullPRMergeableOnTipMove` refuses retained pre-move merge fields. Guards, one shared failure. `TestDispatch_ClosureRecordStillRefusesAPreCloseViewWithoutTheGate` pins that the second one is not made redundant by the first.

Age (`ghda bounds watermarksta.WatermarkRetention`, several days) and live in the disposable cache DB. Losing them costs at most a window in which an out-of-order delivery is applied as it was before any of this existed.
