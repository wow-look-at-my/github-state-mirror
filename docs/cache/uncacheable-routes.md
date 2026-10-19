# Routes deliberately left uncached

CLAUDE.md's operator ruling is total: *"never encourage bypassing the cache for any reason. Bypassing the cache is a hack that means we need something fixed in github-state-mirror."* `docs/cache/rest-routes.md`'s "Still forwarding" section is where an unmodeled route's open work lives, on that ruling. Every entry there is a gap to close, not a verdict.

This doc is the other case: a route that forwards NOT because nobody got to it, but. This is because caching it will be incorrect, circular, or outside what this mirror's cache exists to answer. Each entry below states which of those applies and why a tier-2 row will not help.

## `GET /rate_limit`

No longer left here unmodified: it is now a served route (`internal/api/respcache_ratelimit.go`), just not a traditional tier-2 cache row. The reasoning below is why a **TTL-bounded store row** will be incorrect for it. Why the route is answered from `internal/ratemeter`'s passively observed `X-RateLimit-*` state instead. This entry stays for the "why not a normal cached route" history.

Registered as a real, served route (not left to the NotFound passthrough) purely for backward compatibility with callers that still poll `GET /rate_limit` directly. Their answer is now free and instant instead of a real GitHub round trip that also paid the passthrough debouncer's hold (`PASSTHROUGH_DEBOUNCE`, default 5s). This is backwards for an endpoint whose whole point is telling a caller its standing quickly.

The route sits inside `requireAuth` like every other data route. The identity keying its answer ("user:<id>" for a resolved user token, "app:<id>" for a verified `X-Mirror-Identity` caller, a token fingerprint otherwise) is exactly the identity `h.fetchUpstream`'s meter `Observe` call already accumulates that same principal's cached-route traffic under. Keying on the raw token or an unauthenticated identity instead will answer from a near-always-empty bucket. Since almost all of a principal's GitHub calls are cached-route misses recorded under its resolved actor, not its bare token.

Staleness: a resource with no live observation — never seen, or its window already rolled over — is simply omitted. That Staleness is per `ratemeter.Store.ObservationsFor`'s own window-aware `dead()` rule, not a flat serve TTL. A flat TTL will blank out a caller's perfectly valid standing just because it did not happen to poll again within it. The meter's own rule is the honest bound instead.

The answer changes on **every** API call the credential makes. Including calls this mirror itself proxies on the caller's behalf, and any call the caller makes directly against `api.github.com` outside the mirror fully. No webhook, and no signal of any kind, announces "a call against this token just happened". The mirror cannot even see the direct-to-GitHub calls that will invalidate a cached answer. A TTL-bounded row will serve a stale **budget**, computed as of some past fetch. That row is to a caller deciding whether it is safe to make one more request right now — a correctness regression, not a missing feature.

That is what rules out. A store-backed cache with a TTL, not whether the route can be served at all: `internal/ratemeter` already passively observes the `X-RateLimit-*` headers. On every request that flows through the mirror on this credential's behalf — the passthrough proxy, cached-route miss fetches, show probes, every `ghclient` call. This is for free. This is because the mirror already made that request for its own reasons. That mirror is with no TTL and no upstream round trip of its own needed to answer from it.

The residual gap is real and unfixable from inside the mirror: a call the caller makes **directly** against `api.github.com`. That gap bypases the mirror fully, is invisible to `internal/ratemeter`. A credential used partly through the mirror and partly direct sees the mirror's view lag the real one by however much direct traffic it sends. The dashboard's `GET /api/ratelimit` exposes the same observed state (alongside the App's own live per-installation poll, `internal/sync/consistency.go`) for operators.

## `GET /app/hook/deliveries`

This is GitHub's own webhook-delivery log for the App, an append-only, ever-growing feed with no stable resource to model. Each new delivery is a new entry. "the deliveries so far" is not a fact that has a version to cache and invalidate. It is a view of an ever-moving log, and the value of reading it is accurately how current the view is. There is also no signal that can invalidate a cached page: GitHub does not deliver a webhook announcing "a webhook delivery was recorded". That will be circular.

Consider the exact shape of self-reference `internal/sync/replay.go`'s own delivery-gap replayer. That shape exists to work around by asking the log directly rather than trusting anything cached.

The mirror already has an internal, unrelated consumer of this endpoint. The delivery-gap replayer (`internal/ghclient/app.go`'s `ListFailedDeliveries`, driven by `internal/sync/replay.go` on `WEBHOOK_REPLAY_INTERVAL`) polls it directly via `ghclient`, not through this HTTP route, specifically. This is because it needs the live answer to find deliveries that never reached the mirror. A cached copy of this route will not serve that consumer even if one existed. An external caller hitting this route directly is, like the replayer, asking because they want to know right now — the same reasoning as `/rate_limit`.

## `GET /wp-json/*`

Web scanners probe WordPress paths, and GitHub's API has none. The mirror answers them with GitHub's own Not Found document (`internal/api/notgithub.go`) and spends no upstream call. They are not a cache row, because no state exists to cache.

## `GET /robots.txt`

Not a GitHub REST resource in the sense every other route in this catalogue is — no owner, no repo, no credential dimension. This is and the same static answer for every caller including an unauthenticated one. It carries no webhook signal (GitHub does not announce a `robots.txt` change) and no per-caller authorization question for the show layer to answer. A cache table, key scheme. Invalidation story built for this will be pure ceremony over a resource that sits outside the GitHub domain model this cache exists to mirror. Closer to a static asset than to anything else in this catalogue.

Passthrough debouncing (`PASSTHROUGH_DEBOUNCE`, default 5s. `docs/cache/three-tier-contract.md`) already collapses concurrent same requests for it into one upstream call, which is the only property a cache will added here.
