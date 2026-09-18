# internal/reqtimeline — the Timeline ring

Extracted from CLAUDE.md, where it had grown past the index budget, and
corrected where the enumeration had gone stale. What is charted, and the guard
that keeps it complete, is in
[request-visibility.md](request-visibility.md); this file is the ring itself.

## What it is

An in-memory ring of TIMED traffic events behind the dashboard's "Timeline"
chart.

**Everything the mirror exchanges is on the chart; a gap is a bug** (operator
ruling 2026-07-15 — the first cut scoped out ghclient's background traffic as
"deliberately not recorded" and was REVERTED for it; never reintroduce an
observability exclusion).

## What is recorded

Each event carries its REAL measured duration — never faked to an instant,
never inflated:

- **Every incoming webhook delivery attempt, whatever its outcome.** Verified
  ones from receipt to dispatch-complete; rejected/unverified/unparseable ones
  on the fixed `⇐ (unverified)` / `⇐ (unknown)` lanes — attacker-controlled
  headers never mint lanes, and claimed metadata rides only as clamped tooltip
  detail.
- **Every inbound data-API request the mirror serves** — hits, misses,
  passthroughs, writes, errors — timed end-to-end via `requestLog.observe*`
  off the router's `stampRequestStart` receipt stamp.
- **Every outbound request the mirror sends.** The full inventory and the
  mechanism behind each is in [request-visibility.md](request-visibility.md):
  cached-route miss fetches (`upstream`) and reveal probes (`probe`) from the
  API layer's client transport; the passthrough proxy's mirror→GitHub leg,
  batched or not (`upstream`); every ghclient call (`internal`); the sign-in
  handshake (`login`); the github.com login relays (`relay`); and every
  subscriber-notification attempt on the `⇒ notify` lane, with status, attempt
  number and terminal flag.

## The one uncharted surface

Stated openly: the dashboard's own UI endpoints (`/api/*`, login, assets). The
chart polling itself would recursively fill the chart with the act of viewing
it. If the operator wants those too, add them; don't argue.

## Bounds and behavior

- Read by the admin-only `GET /api/timeline?since=<id>` cursor endpoint.
- Bounded by 24h retention (lazy eviction on write AND read; no background
  timers) plus a 100k count cap.
- Nil-receiver-safe.
- In-memory, on the requestLog/ratemeter stance: a live view, not an audit
  log. It resets on restart.
- **Lanes stay bounded by construction**: `⇐ <event type>` for webhooks,
  `normalizeRoute`'s route shapes for requests and exchanges, `⇒ notify` —
  never per-URL, never from untrusted input.

## `Event` fields (`reqtimeline.go`)

- `ID` is a monotonically increasing sequence number -- the client's merge key and the `?since=` cursor.
- `Lane` is the swimlane this event renders on: `⇐ <event type>` for webhooks, `<METHOD> <route shape>` (`normalizeRoute`'s bounded families) for requests -- never a per-URL lane.
- `Start` is when handling/fetching began; `DurMs` is the real measured duration in milliseconds (0 for a sub-millisecond event -- still its true rounding, never a fabricated instant).
- `Actor` is the caller's principal/label key; `ActorName` its verified display name when one is known (display-only, like the request log).
- `Detail` is a short free-form tooltip line (e.g. an unverified delivery's claimed event type). Metadata only -- never bodies or secrets.
- The Notify fields (`Target`, `Attempt`, `Final`) are the delivery target's host, the 1-based attempt number, and whether this attempt was terminal (success, or the last retry).
- `Recorder.events[head:]` are the live entries, ordered by insertion (≈ end time, since events are recorded at completion) and therefore by ID. `head` is advanced on eviction and the slice compacted periodically so the backing array cannot grow unboundedly.
- `Snapshot.MaxID` is the newest assigned event ID -- the client's next `?since=` cursor. It keeps advancing even when every event has been evicted.
- `SnapshotRange`'s live entries are ordered by END time, so the first candidate is the first whose end is at/after `from`; an event overlaps the range if it starts before the end of it.
