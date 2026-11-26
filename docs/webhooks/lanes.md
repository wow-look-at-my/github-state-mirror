# Apply lanes and the write lock under load

A push burst across the org sends a few deliveries per push and several per CI job. `workflow_job` queued, in progress and completed for every job, and `workflow_run` for every run. One worker applies the inbox. In plain arrival order a pull request or status that lands behind that flood waits for all of it. And every client that reads it sees old state for minutes.

## The lanes

`webhook.LaneOf` puts each delivery in one of two lanes:

- **bulk**: `workflow_job` and `workflow_run`. These carry the most volume and are the least time-critical.
- **client**: every other event type. Clients wait on these.

The inbox reads one lane at a time, oldest first (`inbox.Store.Pending`). The worker (`webhook.Queue.pick`) applies by these rules:

- **Client first.** A recorded client delivery goes before any recorded bulk delivery.
- **Leave the bulk lane at once.** While the worker drains bulk deliveries, a client delivery that arrives waits only for the delivery in hand. The worker then reads the journal again.
- **No starvation.** After `bulkEvery` client deliveries in a row, one bulk delivery goes. A steady client stream still leaves the bulk lane a share of the worker.
- **Arrival order inside a lane.** Each lane keeps the journal's order.
- **A repository delivery drains older bulk first.** A rename, delete, transfer or visibility change deletes or flushes what the bulk deliveries before it write. Each bulk delivery recorded before it is applied first, as arrival order will do. Bulk deliveries recorded after it still wait behind it.

## Why the ordering guarantees hold

Ordering is the watermark's job, not the worker's (docs/webhooks/ordering.md). The watermark subjects of the bulk lane (`workflow_run:`, `workflow_job:`) appear in no client delivery. Each lane keeps arrival order. The watermark therefore sees each subject's deliveries in the order it always did.

What crosses the lanes is the repository object that every payload carries and `absorbRepoFromPayload` upserts. A bulk delivery can now land after a newer push. That repository object is older than the push, and its fields have separate guards:

- `pushed_at` moves only forward. The absorb drops a `pushed_at` older than the stored one (`laterTimestamp`).
- Visibility, archived state, the default branch and the name change only with a `repository` delivery, which drains the bulk deliveries older than it. A bulk delivery recorded later carries the newer state.

Bulk deliveries also write the `workflow_runs` and `workflow_jobs` truth tables, their response caches, and the check-runs pages that a completed run contradicts. No client handler reads those tables to derive what it writes, so a later bulk apply changes no client answer.

## The write lock

The worker's applies, the periodic refresh and the delivery replayer write with `database.WithBackgroundWrites`. The write gate admits foreground writers first, in arrival order: a request absorbing a fetch, a grant write, a probe verdict. A background writer goes when no foreground writer waits, or once it waited `backgroundMaxWait`, so requests cannot starve it either. The webhook worker holds the lock for one delivery at a time. A request therefore waits at most for the transaction already in hand, not behind a queue of deliveries.

## What is measured

The Latency tab splits both waits:

- `webhook.handle.client` and `webhook.handle.bulk`: receipt to applied, per lane. `webhook.handle` stays the total.
- `db.write_wait`: a foreground writer's wait for the write lock. `db.write_wait.background`: a background writer's wait.

A burst shows up as a long `webhook.handle.bulk` tail while `webhook.handle.client` and `db.write_wait` stay short. A long client tail means the client lane alone outruns the worker. The fix there is the apply cost of each delivery, since the lanes cannot help with that.
