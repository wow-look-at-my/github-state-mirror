# internal/ghclient — the GitHub API client

Extracted verbatim from CLAUDE.md, where it grew into a manual inside one bullet. Nothing here is summarized. The index entry now points at this file.

## What it holds

GitHub REST + GraphQL API client. Includes the in-memory token→identity cache behind `ResolveTokenIdentity` and `AppAuthenticator` for GitHub App JWT / installation-token minting. `Installations` paginates past 100.

## Retries (`doJSON`)

`doJSON` — the shared HTTP exchange behind the REST/GraphQL helpers (`GetOrgData`, `GetOwnerData(WithProgress)`, `OwnerRepoVisibilities`, `Installations`, `InstallationToken`, `GetRateLimit`) — **retries transient failures**: 502/503/504/429 and network errors, attempts total with a short backoff (500ms/2s. A parseable `Retry-After` raises the floor but is capped at 10s so a large value cannot wedge a deadline-bounded fetch. Bodies are buffered once so retries resend them).

Deliberately NOT retried: authoritative 4xx answers (the show layer and deny-cache semantics depend on them), the caller's own context cancellation/deadline, and GraphQL-level `errors[]` bodies (HTTP 200 semantic answers, checked by the callers). The passthrough proxy does not go through `doJSON` and is untouched.

## The two GraphQL fetch families

The identity-locked `GetOrgData` (`organization(login:)`, the lazy /graphql route's byte-frozen contract) and the sync/checker-private `GetOwnerData` + `OwnerRepoVisibilities` (`repositoryOwner(login:)` — resolves Users AND Organizations. Selects extras the locked query must never grow: `isArchived`, `labels(first: 100)`, `autoMergeRequest`, the connection's `totalCount` (lets per-page progress reporting say "N of M repos" from the first page), the `visibility` enum. The DATA query now carries per-repo `visibility` too (lowercased in `convertRepo`), so a fleet-refresher/consistency-apply `SyncOrgTruth` STAMPS visibility into truth. This is while `UpsertRepo`'s non-empty guard means the visibility-less locked org query can never blank a stamped value. Before this, refresher-synced rows sat at `''` = fail-closed unknown — the 2026-07-20 report's 203 `visibility_unknown` entries covering essentially every PazerOP/read-only-reference repo).

`OwnerRepoVisibilities` applies no `isArchived` filter, unlike `GetOwnerData`'s `repositories(isArchived: false)`. The consistency checker needs archived repos back too, so a cached repo missing from the org data can be positively classified. This is as archived rather than deleted or renamed. Its response per repo is tiny (name, visibility, isArchived), so it pages 100 at a time.

## `ownerAffiliations: OWNER`, and the foreign-node drop

Both owner queries pin **`ownerAffiliations: OWNER`** — the connection's default `[OWNER, COLLABORATOR]` made a User's listing include repos the login merely *collaborates* on, under their real owners. This `convertRepo`/`convertPR` then keyed by the QUERY login (the collaborator-repo bleed: junk `<user>/<name>` truth rows with foreign `name_with_owner`/`url`, double-counted PRs).

As belt and braces, every repo-conversion loop also **DROPS any node whose real owner differs case-insensitively from the queried login**. That covers the org and owner loops and the visibility loop. The real owner is the `nameWithOwner` prefix, else `owner.login`. Each drop is logged (`dropForeignRepoNode` in `queries.go`). It is a drop, not a re-key, so one owner's sync can never absorb truth rows or mint grants across owners. This also covers the identity-locked org query with zero query-text change (`TestOrgQueryUntouched` still passes) and kills the visibility map's bare-name collision hazard (a same-named foreign node clobbering an owned repo's visibility entry). A deploy-time nuke (schema history, entry 14) cleaned up the junk rows a corrected re-run can never delete (sync never deletes repo rows. The PR reconcile only visits fetched repos). Reconcile, not a nuke, is the tool for that today — the DB is rebuilt only by a schema change now. There is no version to bump for a data problem.

## Query shapes and conversion (`queries.go`)

`orgDataQuery` fetches a page of non-archived repos and the first page of each repo's open PRs. It pages repositories small (`first: 5`) on purpose: requesting all repos at once, each with `statusCheckRollup` on the default branch. This is on every open PR's last commit, produces a response large enough that an intermediary (or GitHub itself) returns "502 Bad Gateway" for active orgs. Small pages keep each response within those limits. The `repoCursor` loop in `GetOrgData` stitches them back together. RepoPRsQuery` (`fetchRemainingPRs`) complets a repo with more than 100 open PRs.

`The owner-agnostic query selects gqlRepo.IsArchived`. The identity-locked `orgDataQuery` never returns it, leaving it `false` — which matches its own `repositories(isArchived: false)` filter anyway. `gqlRepo.Visibility` is likewise owner-agnostic-only, left `""` by the identity-locked query. `UpsertRepo`'s guard only overwrites the stored column with a non-empty value. As a result, an org-query-sourced sync can never blank a known visibility while an owner-sourced sync stamps it. `The owner-agnostic `owne selects gqlPR.AutoMergeRequest`rPRFields`.

`convertPR` lowercases `mergeMethod` (a GraphQL enum) to match the REST `auto_merge.merge_method` values the cache stores and rebuilds — `SyncOrgTruth`'s upsert deliberately does not apply this column. This is from GraphQL-shaped rows (`node_id` is NULL). It is carried for the consistency checker's diff, and apply mode writes it via the explicit `SetPRAutoMergeMethod`.

`realNodeOwner` extracts a fetched repo node's real owner login: the `nameWithOwner` prefix when present, else the node's own `owner.login`. Every repo-listing selection (org, owner, visibility twin) carries at least one of the two, so a real GitHub response always yields a non-empty answer. `""` (possible only for minimal test fixtures) disables the foreign-node guard for that node.

## Observers

`SetRateObserver` installs the passive rate-limit hook. The ctx principal when set reports every response the client sees, else by the credential's shape (`app-jwt` for a JWT, a short `token:<fp12>` fingerprint otherwise — never the raw token).

`SetExchangeObserver` wraps the client's transport so EVERY call it makes — through any helper, present or future. This is one event per real try — is timed onto the dashboard's Timeline chart with the same identity labeling (a gap on that chart is a bug).

## App-level endpoints

`FailedHookDeliveries` / `RedeliverHook` read the App's own log of the webhook delivery failure and ask GitHub to re-send what it can not hand over. Both are JWT-authenticated app-level endpoints. See docs/webhooks/delivery-gaps.md for why they exist and what bounds their use.

## Verified display names (`internal/actor`)

`actor.WithName` carries the actor's VERIFIED display name (a user's login, an app's slug, or an installation's account login) as display-only metadata alongside the partition key, not a key itself. Only set names proven by GitHub's own answers (`ResolveTokenIdentity`, `VerifyAppIdentity`, an installations listing). Never a name derived from an unverified header.

`actor.Short` abbreviates an actor for display and logs. Only opaque hex token fingerprints (longer than multiple chars) are shortened, to their first hex chars. Structured actors — `user:<id>`, `app:<id>`, `app-installation:<id>` — are short and meaningful already, and truncating them will drop significant digits. They are returned whole.

## `RateLimitResponse` (`ratelimit.go`)

`Rate` is GitHub's deprecated top-level alias for `resources.core`. GitHub still sends it on every answer, so the mirror's own rebuild of this endpoint (`internal/api/respcache_ratelimit.go`) sends it too. A caller that reads it must not silently start reading nothing the day it points at the mirror. Omitted when core is not known, not guessed.
