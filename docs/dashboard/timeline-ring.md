# internal/reqtimeline — the Timeline ring

Extracted from CLAUDE.md, where it grew past the index budget, and corrected where the enumeration went stale. What is charted, and the guard that keeps it complete, is in [request-visibility.md](request-visibility.md). This file is the ring itself.

## What it is

An in-memory ring of TIMED traffic events behind the dashboard's "Timeline" chart.

**Everything the mirror exchanges is on the chart. A gap is a bug** (operator ruling — the first cut scoped out ghclient's background traffic as "deliberately not recorded" and was REVERTED for it. Never reintroduce an observability exclusion).

## What is recorded

Each event carries its REAL measured duration — never faked to an instant, not inflated:

- **Every try of the incoming webhook delivery, whatever its outcome.** Verified ones from receipt to dispatch-complete. Rejected/unverified/unparseable ones on the fixed `⇐ (unverified)` / `⇐ (unknown)` lanes — attacker-controlled headers never mint lanes, and claimed metadata rides only as clamped tooltip detail.
- **Every inbound data-API request the mirror serves** — hits, misses, passthroughs, writes, errors — timed end-to-end via `requestLog.observe*` off the router's `stampRequestStart` receipt stamp.
- **Every outbound request the mirror sends.** The full inventory and the mechanism behind each is in [request-visibility.md](request-visibility.md). Cached-route miss fetches (`upstream`) and show probes (`probe`) from the API layer's client transport. The passthrough proxy's mirror→GitHub leg, batched or not (`upstream`). Every ghclient call (`internal`). The sign-in handshake (`login`). The github.com login relays (`relay`). And every subscriber-notification try on the `⇒ notify` lane, with status, try number and terminal flag.

## The one uncharted surface

Stated openly: the dashboard's own UI endpoints (`/api/*`, login, assets). The chart polling itself will recursively fill the chart with the act of viewing it. If the operator wants those too, add them. Do not argue.

## Bounds and behavior

- Read by the admin-only `GET /api/timeline?since=<id>` cursor endpoint.
- Bounded by 24h retention (lazy eviction on write AND read. No background timers) plus a 100k count cap.
- Nil-receiver-safe.
- In-memory, on the requestLog/ratemeter stance: a live view, not an audit log. It resets on start.
- **Lanes stay bounded by construction**: `⇐ <event type>` for webhooks, `normalizeRoute`'s route shapes for requests and exchanges, `⇒ notify` — never per-URL, not from untrusted input.

## `Event` fields (`reqtimeline.go`)

- `ID` is a monotonically increasing sequence number -- the client's merge key and the `?since=` cursor.
- `Lane` is the swimlane this event renders on: `⇐ <event type>` for webhooks, `<METHOD> <route shape>` (`normalizeRoute`'s bounded families) for requests -- never a per-URL lane.
- `Start` is when handling/fetching began. `DurMs` is the real measured duration in milliseconds (0 for a sub-millisecond event -- still its true rounding, not a fabricated instant).
- `Actor` is the caller's principal/label key. `ActorName` its verified display name when one is known (display-only, like the request log).
- `Detail` is a short free-form tooltip line (e.g. an unverified delivery's claimed event type). Metadata only -- never bodies or secrets.
- The Notify fields (`Target`, `Attempt`, `Final`) are the delivery target's host, the 1-based try number, and whether this try was terminal (success, or the last retry).
- `Recorder.events[head:]` are the live entries, ordered by insertion (≈ end time, since events are recorded at completion) and therefore by ID. `head` is advanced on eviction and the slice compacted periodically so the backing array cannot grow unboundedly.
- `Snapshot.MaxID` is the newest assigned event ID -- the client's next `?since=` cursor. It keeps advancing even when every event was evicted.
- END time orders SnapshotRange`'s live entries. As a result, the first candidate is the first whose end is at/after `from`. An event overlaps the range if it starts before the end of it.
