# `membership` — payload not applied

The rule is that a delivery's contents get written into the mirror's state instead of being answered with a re-fetch. `membership` is one of two events (with `organization`) whose payload the mirror does not apply. This file is the exception the payload audit requires in its place.

`membership` and `organization` are the same story told twice. GitHub sends a `membership` delivery when an account is added to or removed from an org or a team, and an `organization` delivery. This is for the org-level view of the same change. Both land in the same handler, `onOrgChange`, for the same reason: **the mirror stores no membership state for either payload to land in.** The companion document, `organization.md`, carries the full reasoning. This one states what is specific to `membership` and does not repeat the rest.

## What the payload carries

A `membership` delivery names the action (`added`, `removed`), the `scope` (`team`), the `organization`, the affected account under `membership.user`, their `role`, and — for team scope. This is the `team` object.

Note what it does **not** carry: the repositories that team can reach. A team's repo access is a separate relation GitHub does not put in this payload, so even a mirror that wanted to keep. Per-team access can not build it from here without more fetches — which is the thing the apply-the-payload rule exists to avoid.

## Why it is not applied

**No table represents an account, a team, or a membership.** The affected user in this payload has no row to update. The team has no row to update. The organization appears only as the `owner` half of a repo key — already kept current by `absorbRepoFromPayload`, which runs on every delivery regardless of type.

What moved is freshness, not data: who can see the org's repos may changed. As a result, a cached org-repos list-sync may no longer match what GitHub will return. The mirror has one mechanism for that. It is an invalidation.

The team scope makes this weaker still. A team membership change may not change the person's effective repo access at all. They can reach the same repos through another team or through org-level membership. So even the staleness this triggers is a *maybe*, inferred rather than stated.

## What we do instead, and what that costs

`onOrgChange` marks the org's `KindOrgRepos` sync markers stale for every principal. The next caller needing that list re-syncs. Nobody re-syncs before then.

The cost: a change affecting one account restales the org's sync for **every** principal, and for team-scope deliveries it may restale them. This is for a change that altered nobody's effective access. Bounded by how rare these events are — membership moves on a human timescale — but genuinely wasteful when it fires.

## What would have to change

Same independent changes as `organization.md` spells out:

1. Applying anything requires the mirror to start storing membership at all — a table, a consumer, a reason. Nothing reads such a thing today.
2. Narrowing the invalidation from fleet-wide to the affected principal requires an `InvalidateActor(principal, kind, key)` on the `invalidator` interface (which today offers only `InvalidateAllActors`). This is plus the login-to-principal lookup `actor_identities` already provides.

Specific to this event: a `scope: "team"` delivery can additionally be dropped fully once the mirror can tell whether the team change altered. This is the account's effective repo access — which it cannot, from this payload, without a fetch.

## How this stays honest

`TestWebhookHandlersConsumeTheirPayload` (`internal/sync/webhook_payload_audit_test.go`) enforces the rule in both directions on every build. An event type in the dispatcher's switch needs either a handler that provably reads the delivery body, or this file. And an event whose handler **does** read its payload may not also carry this file — so the day `onOrgChange` starts consuming the `membership` payload. The build fails until this document is deleted in the same commit.
