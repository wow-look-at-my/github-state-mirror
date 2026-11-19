## How to model one of these (the tier-2 contract)

Every data route is in exactly one tier. Tier 1 is the `/graphql` org-repos query (byte-identical to GitHub, identity-test-locked — do not touch). Tier 3 is the verbatim passthrough proxy, which is where everything above currently lives. Adding a cached route means tier 2, whose contract is:

1. **Show gate first.** `h.reveal(r, owner, repo, denyKind…, resourceKey)` before any read: public repo / live grant / cached deny verdict / live probe. A denied caller gets GitHub's own relayed answer, an unrevealable transient error 502s that one request.
2. **Absorb state, rebuild trimmed — never byte-replay.** Parse the response into structured columns and render the answer yourself. Drop every URL field. The recursive `assertNoURLKeys` test enforces it, and each exception needs a fresh consumer survey (check the Callers line above for who to survey).
3. **Shape guard.** Model an explicit set of query parameters and only the default JSON `Accept`. Everything else calls `h.passthrough(w, r, PassQuery|PassAccept|PassPath)` so the decline is counted with a reason instead of vanishing.
4. **Same bytes on hit and miss.** Render once at absorb time and store the rendered document, or rebuild identically from rows. Mark responses `X-GSM-Cache: hit|miss`.
5. **Explicit invalidation + TTL + pruning.** Name the webhook event that moves this resource and flush on it in `internal/sync/webhook.go` (per-resource where the payload names it, repo-wide otherwise). Plus a TTL backstop for missed deliveries and LRU pruning against `ghdata.CacheMaxRows`. When no delivery names this resource today, the MISSING SIGNAL is the thing to fix. Subscribe the App to the event, widen an existing flush, or absorb only the answers some signal does cover. Leaving the route forwarding is not one of the options.
6. **Only cacheable answers absorb.** A 200 whose body the model can hold, plus any authoritative verdict worth caching (a 404 "absent" answer frequently is). Transient failures (5xx, 429) relay unstored, every time.

### Where the response shape comes from

The skeletons above are what THIS fleet actually received — the authority on which answers really occur. These query shapes callers send, and who breaks if the rebuild trims a field. They are not the only source: GitHub publishes an OpenAPI description of every documented endpoint (`github/rest-api-description`, `descriptions/api.github.com/api.github.com.json`). This gives the full schema including fields this traffic happened not to exercise, and their nullability. For a documented endpoint, read the spec AND the capture. A route with no captured skeleton is not necessarily an undocumented one.

### Files a new route touches

- `internal/database/schema.sql` — the snapshot table (+ unique key and LRU index). There are no migrations and nothing to declare: adding the table changes the file's scrubbed fingerprint. The deploy nukes and recreates the cache by itself.
- `internal/database/queries/respcache.sql` — get/upsert/touch/invalidate/expire/prune, then `sqlc generate`.
- `internal/ghdata/respcache_<route>.go` — the store methods.
- `internal/api/respcache_<route>.go` — the handler (guard → show → hit → fetch → absorb → rebuild) and `internal/api/router.go` — the registration.
- `internal/sync/webhook.go` — the invalidation, wired into `invalidateResponseCaches` / `invalidateForPush`.
- Tests: a hit/miss pair, the shape-guard passthroughs, the invalidation, and the no-URL-keys assertion. Then `just build && go-toolchain`.
