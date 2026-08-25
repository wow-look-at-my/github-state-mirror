# internal/ratemeter — passive rate-limit observation

Extracted verbatim from the package doc comment and per-field/per-function comments in `ratemeter.go`.

## What it is

GitHub attaches rate-limit headers to every API response, so the mirror can know each credential's standing
for free — no `GET /rate_limit` polling spend. Every upstream path (the passthrough proxy, cached-route miss
fetches, the reveal probe, and ghclient's own calls) reports its responses here, and the admin dashboard's
"Rate limit" tab reads the snapshot.

The store is deliberately in-memory — the same live-view-not-audit-log stance as the api package's request
log: rate-limit windows reset within the hour, so the data is sub-hour-ephemeral, and persisting it would add
a schema.sql table, whose every edit nukes the whole cache. It resets on restart.

## Dead observations age out

An actively observed identity is re-observed with a fresh future reset on every request, so an entry whose
reset moment has passed belongs to an identity that stopped calling — it is pruned. An entry with no usable
reset (a zero Reset) has no window to judge by and is instead pruned once its ObservedAt is older than
staleTTL. Pruning is lazy — swept on Observe (write) and Snapshot (read), the kv package's lazy-expiry
stance — with no background goroutine or timer; the maxEntries cap stays as the size backstop.

`dead(o, now)`: an actively observed identity is re-observed with a fresh future reset on every request; only
an identity unseen since its window rolled over keeps a past reset — so a reset strictly in the past marks the
entry dead. A zero (or negative — garbage) Reset has no window to judge by and instead dies once ObservedAt is
more than staleTTL old. A zero reset must neither mean immortal nor mean instant death.

## Bounds

`maxEntries` (512) bounds the map: unbounded distinct identities (e.g. rotating token fingerprints) must not
grow it forever. On overflow the entry with the oldest ObservedAt is evicted (`evictOldestLocked`, called with
the lock held, only when the map is at capacity, so a linear scan is fine).

`staleTTL` (one hour) bounds observations whose Reset is unknown (zero — header absent or unparseable): with
no reset moment to judge deadness by, an entry unseen for longer than the longest standard rate window is
dead.

## Observation fields

`Identity` labels WHO was consuming the limit: the request's principal (`user:<id>`, `app:<id>`,
`app-installation:<id>`, a token fingerprint) or a credential-derived label (`app-jwt`, `token:<fp12>`). Never
a raw token value.

`Name` is the identity's verified display name (a user login, an app slug, an installation's account login)
when the caller supplied one. Display-only; never part of the (identity, resource) key. A later observation
without a name keeps the last known one — the identity is the same principal either way. In `Observe`: the
(identity, resource) key already pins the principal, so a nameless reading is the same actor seen through a
path that didn't carry the name.

`Resource` is the GitHub rate-limit bucket the reading is for (`X-RateLimit-Resource`: "core", "graphql",
"search", ...). "core" when the header is absent.

`Reset` is when the window resets (`X-RateLimit-Reset`, Unix epoch seconds). `ObservedAt` is when the response
carrying this reading was seen.

## Observe

Parses the `X-RateLimit-*` headers off the response and records the reading under identity. A response
carrying neither `X-RateLimit-Limit` nor a usable `X-RateLimit-Remaining` (304s, non-API hosts, ...) is
ignored — a partial reading is garbage, so both must parse. `X-RateLimit-Used` is derived as
limit-remaining when absent. Last write wins: Observe runs at response time, so the latest call is the
freshest reading.

## Store

Holds the latest observation per (identity, resource). All methods are safe for concurrent use and no-op on a
nil `*Store` (the nil-recorder pattern), so wiring may pass a nil meter without guards. `now` is the clock, a
test seam set once at construction (`time.Now`); tests override it before use.

## ObservationsFor

Returns the live observations for one identity, across every resource GitHub reports for it (core, search,
graphql, ...), sorted by resource. Dead entries are pruned first, exactly as Snapshot does. Empty when the
identity has made no request the meter has observed yet, or its last reading's window has since rolled over —
never a stale answer, since `dead()` is the same window-aware rule Snapshot relies on rather than a flat serve
TTL (a flat TTL would blank out a caller's perfectly-valid standing just because they happened not to poll
again within it).

This is what `GET /rate_limit` is answered from (`respcache_ratelimit.go`): the mirror already learns a
credential's standing for free off every response it makes on that credential's behalf, so a caller asking for
it needs no upstream call of its own. See docs/cache/uncacheable-routes.md for why this is a served route
rather than a traditional tier-2 cache row.
