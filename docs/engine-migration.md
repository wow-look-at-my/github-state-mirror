# Moving a route onto the embedded engine

`internal/engine/github.xml` is an api-mirror spec. The engine serves every route it declares, from its own database file (`DB_PATH` with `-engine.db`). The router sends a request to the engine when `Declares` answers true for it, and keeps every other request. A route lives in exactly one place: the spec, or a hand-written handler.

## Checklist for the next route

1. Find the route in api-mirror's `samples/github/github.xml`. Copy its resource, route and event declarations into `internal/engine/github.xml`, trimmed to this route.
2. Make the spec match the old contract. The TTL, the `<param>` ranges (page caps included), the `<absorb status=>` codes, and `<drop key="url"/>` plus `<drop key="*_url"/>` for the URL keys the old rebuild dropped.
3. Delete the `r.Get` line in `internal/api/router.go`. Do not add a line for the engine: the dispatch in `internal/api/engine.go` finds the route from the spec.
4. Delete the handler, its `ghdata` store code, its `denyKind*` and `RepoAnswerKind*` constants, and every flush of its rows in `internal/sync`. Keep a helper that another route still calls.
5. Port the handler's tests to the real router with the engine mounted (`newFullTestStack` builds both). Deliveries go through `postWebhookJSON`, which waits until the engine has them. Each delivery needs the clock field its `<event>` names.
6. Update the route's entry in `docs/cache/rest-routes.md` and its rows in `docs/webhooks/invalidations.md`.
7. Leave a table in `schema.sql` when another route still reads it. A table that nothing reads goes, with `sqlc generate`.

## What differs between the two implementations

| | Hand-written handler | Engine |
|---|---|---|
| Storage | `github-mirror.db` | its own file, rebuilt when its derived schema changes |
| Grants and denials | `access_grants`, `deny_cache`; a list-sync grants | the engine's own tables; a probe earns each grant again |
| Public fast path | the `repos` truth row | only when the spec declares a `<public>` predicate on a resource it stores |
| Passthrough header | no `X-GSM-Cache` | `X-GSM-Cache: passthrough` |
| Request log disposition of a refusal | `hit` or `miss` | `denied` (reveal) or `refusal` (an absorbed 404) |
| Rate-limit tab | every upstream answer's `X-RateLimit-*` | the engine's calls are absent: its exchange tap carries no headers |
| Caller name on the Timeline | the verified login or slug | the principal key only |
| Delivery | applied in the webhook worker | forwarded after the worker's apply, then ordered by the engine's own watermark |

## The tags listing

`GET /repos/{owner}/{repo}/tags` is the first route on the engine. Differences from the old handler:

- A push that moves an existing tag drops the stored pages. The handler wrote the new sha into each page. The spec language has no apply into one item of a stored document list.
- A page whose entries lack a name or a full sha is stored. The handler passed such an answer through unstored.
- A `repository` delivery drops the repository's pages through an `<event>` of its own.
