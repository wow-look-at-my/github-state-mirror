# internal/ghclient — the GitHub API client

Extracted verbatim from CLAUDE.md, where it had grown into a manual inside one
bullet. Nothing here is summarized; the index entry now points at this file.

## What it holds

GitHub REST + GraphQL API client. Includes the in-memory token→identity cache
behind `ResolveTokenIdentity` and `AppAuthenticator` for GitHub App JWT /
installation-token minting; `Installations` paginates past 100.

## Retries (`doJSON`)

`doJSON` — the shared HTTP exchange behind the REST/GraphQL helpers
(`GetOrgData`, `GetOwnerData(WithProgress)`, `OwnerRepoVisibilities`,
`Installations`, `InstallationToken`, `GetRateLimit`) — **retries transient
failures**: 502/503/504/429 and network errors, 3 attempts total with a short
backoff (500ms/2s, a parseable `Retry-After` raises the floor but is capped at
10s so a huge value can't wedge a deadline-bounded fetch; bodies are buffered
once so retries resend them).

Deliberately NOT retried: authoritative 4xx answers (the reveal layer and
deny-cache semantics depend on them), the caller's own context
cancellation/deadline, and GraphQL-level `errors[]` bodies (HTTP 200 semantic
answers, checked by the callers). The passthrough proxy does not go through
`doJSON` and is untouched.

## The two GraphQL fetch families

The identity-locked `GetOrgData` (`organization(login:)`, the lazy /graphql
route's byte-frozen contract) and the sync/checker-private `GetOwnerData` +
`OwnerRepoVisibilities` (`repositoryOwner(login:)` — resolves Users AND
Organizations; selects extras the locked query must never grow: `isArchived`,
`labels(first: 100)`, `autoMergeRequest`, the `visibility` enum — the DATA
query now carries per-repo `visibility` too (lowercased in `convertRepo`), so a
fleet-refresher/consistency-apply `SyncOrgTruth` STAMPS visibility into truth,
while `UpsertRepo`'s non-empty guard means the visibility-less locked org query
can never blank a stamped value; before this, refresher-synced rows sat at `''`
= fail-closed unknown — the 2026-07-20 report's 203 `visibility_unknown`
entries covering essentially every PazerOP/read-only-reference repo).

## `ownerAffiliations: OWNER`, and the foreign-node drop

Both owner queries pin **`ownerAffiliations: OWNER`** — the connection's
default `[OWNER, COLLABORATOR]` made a User's listing include repos the login
merely *collaborates* on, under their real owners, which
`convertRepo`/`convertPR` then keyed by the QUERY login (the collaborator-repo
bleed: junk `<user>/<name>` truth rows with foreign `name_with_owner`/`url`,
double-counted PRs).

Belt-and-braces, every repo-conversion loop (org and owner alike, plus the
visibility loop) also **DROPS any node whose real owner** (`nameWithOwner`
prefix, else `owner.login`) **differs case-insensitively from the queried
login**, logging each drop (`dropForeignRepoNode` in `queries.go`) — drop,
never re-key, so one owner's sync can never absorb truth rows or mint grants
across owners; this also covers the identity-locked org query with zero
query-text change (`TestOrgQueryUntouched` still passes) and kills the
visibility map's bare-name collision hazard (a same-named foreign node
clobbering an owned repo's visibility entry). A deploy-time nuke (schema
history, entry 14) cleaned up the junk rows a corrected re-run could never
delete (sync never deletes repo rows; the PR reconcile only visits fetched
repos). Reconcile, not a nuke, is the tool for that today — the DB is rebuilt
only by a schema change now, so there is no version to bump for a data problem.

## Observers

`SetRateObserver` installs the passive rate-limit hook: every response the
client sees is reported to `internal/ratemeter`, labeled by the ctx principal
when set, else by the credential's shape (`app-jwt` for a JWT, a short
`token:<fp12>` fingerprint otherwise — never the raw token).

`SetExchangeObserver` wraps the client's transport so EVERY call it makes —
through any helper, present or future, one event per real attempt — is timed
onto the dashboard's Timeline chart with the same identity labeling (a gap on
that chart is a bug).

## App-level endpoints

`FailedHookDeliveries` / `RedeliverHook` read the App's own webhook delivery
failure log and ask GitHub to re-send what it could not hand over. Both are
JWT-authenticated app-level endpoints; see docs/webhooks/delivery-gaps.md for
why they exist and what bounds their use.
