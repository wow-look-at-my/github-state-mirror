# `organization` — payload not applied

The rule is that a delivery's contents get written into the mirror's state
instead of being answered with a re-fetch. `organization` is one of two events
(with `membership`) whose payload the mirror does not apply, and this file is
the exception the payload audit requires in its place.

The short version: **the mirror stores no organization state for this payload
to land in.** It is not that applying it would be inconvenient — there is no
row it corresponds to.

## What the payload carries

An `organization` delivery names an action (`member_added`, `member_removed`,
`member_invited`, `renamed`, `deleted`, ...), the `organization` object
(login, id, description, avatar), and — for the member actions — the account
affected, under `membership.user` or `member`.

## Why it is not applied

Look at what this service actually persists. The truth tables are `repos`,
`pull_requests`, `pr_labels`, `commit_checks`, `workflow_runs` and the
response caches keyed off them. **There is no organization table, no
membership table, and no per-org row.** An organization's login appears only
as the `owner` half of a repo's key, and the member accounts in this payload
appear nowhere at all: the mirror has never stored who belongs to an org,
because nothing it serves is derived from that.

So there is no upsert to write. The `organization` object in the payload
duplicates fields already absorbed from every delivery's own `repository`
object — `absorbRepoFromPayload` runs on every event and keeps the owner login
and avatar on the repo rows current — and the member accounts have no
representation to update.

What a membership change genuinely moves is not state, it is **freshness**:
who may see the org's repos has changed, so a principal's cached org-repos
list-sync may no longer describe what GitHub would return them. That is a
staleness fact, not a data fact, and the mirror has exactly one mechanism for
it.

## What we do instead, and what that costs

`onOrgChange` marks the org's `KindOrgRepos` sync markers stale for every
principal (`InvalidateAllActors`). The next caller who needs that list
re-syncs it; nobody re-syncs until then.

The cost is honest and bounded, but it is real: this is a **fleet-wide**
invalidation triggered by a change affecting **one account**. Every principal
that has ever synced this org re-syncs, not just the person who joined or
left. The mirror cannot narrow it, because narrowing needs a mapping from the
payload's login to the principals holding that org's sync markers, and
`invalidator` exposes only `InvalidateAllActors(kind, key)` — one key, all
actors, by construction.

These events are also rare. An organization's membership changes on a human
timescale, not a CI timescale, so the wasted re-syncs are measured in a
handful per week against a delivery log that turns over in minutes.

## What would have to change

Two separate things, and they are worth naming so the next person does not
have to re-derive them:

1. **To apply anything at all**, the mirror would first have to start storing
   org membership — a table, a reason to keep it, and something served from it.
   Nothing today reads such a thing. Adding a table purely so this handler has
   somewhere to write would be the tail wagging the dog.
2. **To stop the fleet-wide invalidation** (the more useful of the two), the
   `invalidator` interface needs a per-principal narrowing —
   `InvalidateActor(principal, kind, key)` — plus a login-to-principal lookup,
   which `actor_identities` already provides (it maps a principal to the login
   GitHub verified for it). Then `member_removed`/`member_added` would restale
   only the affected principal's marker. That is a genuine improvement and it
   does use the payload; it is simply not the same thing as applying state, and
   it is not implemented today.

A cheaper partial step, also unimplemented: `member_invited`, `renamed` and
`edited` cannot change what any account may read, so those actions could be
answered with no invalidation at all rather than the current unconditional one.

## How this stays honest

`TestWebhookHandlersConsumeTheirPayload` (`internal/sync/webhook_payload_audit_test.go`)
checks this in **both** directions on every build:

- Every event type in the dispatcher's switch must either have a handler that
  provably reads the delivery body — followed through delegation, by AST walk —
  or a file at `docs/webhooks/payload-unused/<event>.md`.
- An event whose handler **does** read its payload may not also carry that
  file. So if anyone implements either change above, the build fails until this
  document is deleted in the same commit. The exception cannot outlive the
  reason for it.

The audit also refuses a stub: this file must answer each of the four headings
above and be substantial, because an exception nobody can explain at length is
usually a handler that should be applying its payload.
