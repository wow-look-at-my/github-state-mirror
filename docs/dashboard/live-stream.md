# The dashboard's push channel

The dashboard does not poll for data. The server tells the page what changed over one Server-Sent Events connection, `GET /api/stream`, and the page loads only the thing that changed.

## The stream

`internal/api/live.go` holds all of it. `LiveHub` fans a change signal out to every open stream. A signal is one bit per topic. A subscriber holds a bitmask and nothing else. A slow browser therefore costs the server no memory, and a publish never blocks.

The topics are the dashboard's data sources:

| Topic | Raised by | Tab |
| --- | --- | --- |
| `requests` | `requestLog.recordFull` | Requests |
| `webhooks` | a webhook event on the timeline ring | Webhooks |
| `timeline` | any event on the timeline ring | Timeline |
| `ratelimit` | `ratemeter.Store.Observe` | Rate limit |
| `latency` | `latency.Recorder.Observe` | Latency |
| `cache` | a released write lock (`db.write_hold`) | My cache, Principals |

`LiveHub.watch` installs the hooks on the in-memory recorders. A hook runs outside the recorder's lock. The `cache` topic rides on the write lock because a finished write is the signal that cached state changed. A read raises nothing.

A frame is one JSON object on a `data:` line:

```json
{"now": 1731700000000, "topics": ["requests", "timeline"],
 "requests": {"events": [], "total": 1842, "by_disposition": {"hit": 1503}}}
```

The first frame carries `boot`, a random id for the process. A burst of changes is held for `liveFlushInterval` and sent as one frame. An idle stream sends an empty frame every `liveHeartbeat`.

Any signed-in user may open the stream. A non-admin receives the `cache` topic only. Request events and the other topics go to an admin.

Shutdown closes the hub (`srv.RegisterOnShutdown(live.Close)` in `cmd/server`). A stream never ends by itself, and `http.Server.Shutdown` waits for every connection.

## The request log rides the stream

The request ring keeps `requestLogRecentCap` events, and each event has a `seq`. A stream holds a cursor into that ring, not a queue. When `requests` is raised, the stream reads the events after its cursor and sends them. A slow client reads later from the same ring. `missed` reports events that left the ring before the stream reached them.

`GET /api/requests` takes parameters for the same cursor:

- `?limit=<n>` sizes `recent`. The default stays small.
- `?since=<seq>` cuts `recent` to the events after that cursor.

The page opens the stream with `?requests_since=<seq>` after a reconnect, so it loses nothing while the connection was down.

`seq` restarts with the process. The page compares `boot` and empties its log when the value changes.

## The page

`app.ts` owns the one `EventSource`. On a frame it does things:

1. It hands the request events to `<gsm-request-log>`.
2. It dispatches a window `gsm:stream` event. `<gsm-timeline>` listens and reads `/api/timeline?since=` when the ring grew.
3. It loads the active tab again when the tab's topic is in the frame.

A tab loads at once when it was idle. While changes keep coming it loads no more frequently than its entry in `VIEW_FEEDS` allows. The cache tabs run SQL counts, so their floor is the highest.

Timers remain, and neither fetches data:

- A watchdog checks that a frame arrived inside `STREAM_STALL_MS`. A silent stream is closed and opened again.
- `<rate-meter>` redraws its countdown text.

A lost connection is retried every `STREAM_RETRY_MS`, forever. The pill in the page head says `Live` or `Reconnecting`. A dead feed is never mistaken for a quiet one. The Timeline's own staleness marking uses the same heartbeat.

## GitHub calls behind a pushed tab

A pushed tab loads many times a minute under traffic. Two tabs call GitHub on a load, so `ttlValue` holds those answers for a short time:

- the Rate limit tab's per-installation poll, for `liveRatesTTL`.
- the Webhooks tab's App subscription list, for `subscribedTTL`.

Without the hold, the rate-limit poll feeds itself: its own response headers raise `ratelimit`, and that loads the tab again.

## The log element

`<gsm-request-log>` (`web/src/request-log.ts`) is created once and kept for the life of the page. It fills on every tab.

- It holds the newest `LOG_CAP` events. The oldest leave first.
- By default it folds same requests into one row with a count. Same means the same method, path, caller, result, upstream status and reason.
- The default order is first-seen, newest kind on top. A busy row counts up in place and never moves. Other orders are offered.
- A text filter, result toggles, Pause and Clear narrow or hold the view.
- It draws one page of rows, with "Show more". The DOM size does not depend on the log size.
- Rows are keyed. A render touches the cells that changed and moves no other node.
- A render that ran long pushes the next one back, so drawing never takes most of the main thread.

## Tests

- `live_test.go` covers the stream, the cursor, the topic mapping and the admin gate.
- `TestRequestLogModel` runs the built log model under node, including its cost with a full buffer.
- `testdata/dashboardcheck.test.ts` (`just test-dashboard`) starts the real server and a real Chromium. It requires that a request reaches the page at once, that an idle page makes no request, that a flood loses no event. It blocks no frame, and that a dead server is reported.
