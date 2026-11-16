# Moving a route onto the embedded engine

`internal/engine/github.xml` is an api-mirror spec. The engine serves every route it declares, from its own database file (`DB_PATH` with `-engine.db`). The router sends a request to the engine when `Declares` answers true for it, and keeps every other request. A route lives in exactly one place: the spec, or a hand-written handler.

The router passes the engine the principal `requireAuth` resolved. The spec therefore declares no `<identity>` and no `<whoami>`, and the engine asks GitHub nothing about who asks.

## Checklist for the next route

1. Find the route in api-mirror's `samples/github/github.xml`. Copy its resource, route and event declarations into `internal/engine/github.xml`, trimmed to this route.
2. Make the spec match the old contract. The TTL, the `<param>` ranges (page caps included), the `<absorb status=>` codes, and `<drop key="url"/>` plus `<drop key="*_url"/>` for the URL keys the old rebuild dropped.
3. Delete the `r.Get` line in `internal/api/router.go`. Do not add a line for the engine: the dispatch in `internal/api/engine.go` finds the route from the spec.
4. Delete the handler, its `ghdata` store code, its `denyKind*` and `RepoAnswerKind*` constants, and every flush of its rows in `internal/sync`. Keep a helper that another route still calls.
5. Port the handler's tests to the real router with the engine mounted (`newFullTestStack` builds both). Deliveries go through `postWebhookJSON`, which waits until the worker applied them. The worker applies each to the engine too. Each delivery needs the clock field its `<event>` names.
6. Update the route's entry in `docs/cache/rest-routes.md` and its rows in `docs/webhooks/invalidations.md`.
7. Leave the route's table in `schema.sql`, even when nothing reads it any more. Dropping it changes the generated queries, and a branch push may not change a generated file. Delete only the hand-written store code.

## What differs between the two implementations

| | Hand-written handler | Engine |
|---|---|---|
| Storage | `github-mirror.db` | its own file, rebuilt when its derived schema changes |
| Grants and denials | `access_grants`, `deny_cache`; a list-sync grants | the engine's own tables; a probe earns each grant again |
| Public fast path | the `repos` truth row | only when the spec declares a `<public>` predicate on a resource it stores |
| Passthrough header | no `X-GSM-Cache` | `X-GSM-Cache: passthrough` |
| Request log disposition of a refusal | `hit` or `miss` | `denied` (reveal) or `refusal` (an absorbed 404) |
| Caller name on the Timeline | the verified login or slug | the principal key only |
| Delivery | applied in the webhook worker | applied in the same worker step, before the inbox marks the delivery applied, and ordered by the engine's own watermark. An engine refusal makes the delivery an `error` |

## The tags listing

`GET /repos/{owner}/{repo}/tags` is the first route on the engine. Differences from the old handler:

- A page whose entries lack a name or a full sha is stored. The handler passed such an answer through unstored.
- A `repository` delivery drops the repository's pages through an `<event>` of its own.

## The git tree

`GET /repos/{owner}/{repo}/git/trees/{sha}` is on the engine. Differences from the old handler:

- A row is refetched after the resource's long TTL. The handler kept a row until the LRU prune.
- The engine keeps GitHub's answer whole, less every `url` key. The handler rebuilt a fixed set of fields. For the documented tree shape the two are the same fields.
- An answer with no `sha` is stored. The handler passed it through.
- A short sha or a branch name does not match the route, so it reaches the proxy with the reason `unrouted`. The handler forwarded it with the reason `unmodeled-path`.
- The `git_trees_cache` table stays in the schema and nothing reads it.

## The branch rules

`GET /repos/{owner}/{repo}/rules/branches/{branch}` is the second route on the engine. Differences from the old handler:

- The key is the branch as the path escapes it. The handler unescaped it, so `a/b` and `a%2Fb` were one row. They are separate rows now, each holding GitHub's answer.
- A branch that is too long or holds a control byte reaches the router's fallback, which forwards it with the reason `unrouted` and without `requireAuth`. The handler ran behind `requireAuth` and forwarded it with the reason `unmodeled-path`.
- The engine stores any 200 array. The handler passed through an answer whose entries lacked a `type` or a positive `ruleset_id`.
- The handler kept the fields of each rule and `parameters` whole. The engine keeps every field and drops each URL key at any depth, inside `parameters` too.
- A 404 is stored for every caller for the TTL. The handler remembered it per caller for the deny window.
- A 403 from the fetch makes the engine ask the repository probe again. A passing probe keeps the caller's proof. The refusal is remembered for that token, branch and page for the deny window, as the handler did. A refused probe denies the caller the repository on every engine route.
- A 5xx puts the key in a backoff, and a request inside it answers 502 without an upstream call. The handler relayed every 5xx and asked again on the next request.
- A `repository` delivery still drops the repository's rows. The engine needs an `<event>` of its own for it, where the handler shared the repo-wide flush.
- This service ignores a `repository_ruleset` delivery now. The applier reports the engine's outcome for a delivery this service ignores, so the Timeline and the subscriber notifiers see `invalidated`. The applier also rewrites the delivery log row this service wrote inside its own apply. The Webhooks tab reads `invalidated` too.
