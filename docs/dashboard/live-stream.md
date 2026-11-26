# The dashboard's push channel

The dashboard does not poll for data. The server pushes the data itself over one Server-Sent Events connection, `GET /api/stream`. The page asks for nothing after it opens that stream.

A stream that only names what changed is not enough. The page then reads the tab's endpoint again on each change. Under steady traffic that is a poll at the page's own pace. So a frame carries the data, not a notice to go and read it.

## The stream

`internal/api/live.go` holds the stream, and `live_feeds.go` holds what it carries for each tab.

The page opens the stream for one tab: `GET /api/stream?view=<tab>`. It opens the stream again when the user changes tab. The first push of a stream is the tab's whole data. A tab needs no request of its own. Each later push is what changed.

| `view` | Raised by | What a push carries | Pace |
| --- | --- | --- | --- |
| `mine`, `all` | a released write lock (`db.write_hold`) | the cache payload, whole | `livePaceCache` |
| `webhooks` | a webhook event on the timeline ring | the delivery rows that are new or changed | `livePaceEvents` |
| `timeline` | any event on the timeline ring | the ring's events after the chart's cursor | `livePaceEvents` |
| `requests` | `requestLog.recordFull` | the counters and the group tables | `livePaceRequests` |
| `ratelimit` | `ratemeter.Store.Observe` | the rate-limit payload, whole | `livePaceAggregate` |
| `latency` | `latency.Recorder.Observe` | the histograms, whole | `livePaceAggregate` |

`LiveHub` fans a change signal out to the open streams. A signal is one bit per topic. It never leaves the server. It wakes the goroutine that builds the tab's data. A subscriber hears only the topic of its own tab. A publish never blocks, and a slow browser costs the server no memory.

`LiveHub.watch` installs the hooks on the in-memory recorders. A hook runs outside the recorder's lock. The cache tabs ride on the write lock because a finished write is the signal that cached state changed. A read raises nothing.

`pumpView` builds a tab's data off the stream's own loop. A slow build holds no request event back. It builds once when the stream opens and again each time the topic is raised. The pace is the least time between a pair of builds: it bounds the cost of a tab under a flood. It is not a timer. A build that changed nothing sends nothing, so an idle server sends only the heartbeat.

A frame is one JSON object on a `data:` line:

```json
{"now": 1731700000000,
 "requests": {"events": [], "total": 1842, "by_disposition": {"hit": 1503}},
 "view": {"name": "webhooks", "data": {"window": 100, "deliveries": []}}}
```

The first frame carries `boot`, a random id for the process. A burst of changes is held for `liveFlushInterval` and sent as one frame. An idle stream sends an empty frame every `liveHeartbeat`. When a build fails, `view.error` replaces `view.data`, and the page shows it.

Any signed-in user may open the stream for `mine`. Every other view, and the request events, go to an admin only. The server answers 403 to a non-admin that names one, and 400 to an unknown view.

## The delivery log is a delta

A Webhooks push holds the rows that are new, and the rows whose outcome changed. The engine amends a delivery's outcome after the dispatcher logged it. That row is then sent again. The feed reads the log's newest `webhookWindow` rows and compares them with what this stream sent. It reads committed rows only, so a rolled-back delivery never reaches the page.

The first push sets `reset`. The page drops what it holds. The page keeps the newest `window` rows by `id`. The ordering counters, the out-of-order samples and the missing subscriptions are each sent only when they changed. An absent field means "no change".

## The Timeline is a delta

The chart reads the hour it paints one time, from `GET /api/timeline`. It then names the newest event it holds: `?view=timeline&timeline_since=<id>`. From there each push is the ring's events after that cursor, in the same columnar format the endpoint serves. An SSE frame is text, so the page rides in base64.

A frame names the cursor it continues from (`since`). The chart refuses a frame that starts past its own cursor and asks for the stream again. It does the same after a hidden page dropped frames. `liveTimelineBatch` bounds one frame. A longer catch-up arrives as several frames, and each one moves the cursor only as far as it reached. A cursor past the ring's newest id is from an earlier process.

The feed starts again from the first event.

Shutdown closes the hub (`srv.RegisterOnShutdown(live.Close)` in `cmd/server`). A stream never ends by itself, and `http.Server.Shutdown` waits for every connection.

## The request log rides the stream

The request ring keeps `requestLogRecentCap` events, and each event has a `seq`. A stream holds a cursor into that ring, not a queue. When `requests` is raised, the stream reads the events after its cursor and sends them. A slow client reads later from the same ring. `missed` reports events that left the ring before the stream reached them.

`GET /api/requests` takes parameters for the same cursor:

- `?limit=<n>` sizes `recent`. The default stays small.
- `?since=<seq>` cuts `recent` to the events after that cursor.

The page reads the server's whole ring one time, when the Requests tab first opens. It opens each stream with `?requests_since=<seq>`, so it loses nothing across a tab change or a lost connection.

`seq` restarts with the process. The page compares `boot`. When the value changes it empties its log, reads the ring again and builds a new Timeline chart.

## The page

`app.ts` owns the one `EventSource`. On a frame it does things:

1. It hands the request events to `<gsm-request-log>`.
2. It dispatches a window `gsm:stream` event. `<gsm-timeline>` takes it as proof that the feed is alive.
3. It draws the active tab from `view.data`. A Timeline push goes to `<gsm-timeline>`, which merges it into the chart.

A hidden page and an open modal hold the draw. The newest data is drawn when they end.

The requests the page still makes are set-up, or an act of the user:

- `/api/me`, one time.
- The request ring and the Timeline's first hour, one time each. A failed read is retried at a fixed interval until it loads.
- An older Timeline range, when the user pans to it.
- A modal, and the Copy JSON control.

Timers remain, and neither fetches data:

- A watchdog checks that a frame arrived inside `STREAM_STALL_MS`. A silent stream is closed and opened again.
- `<rate-meter>` redraws its countdown text.

A lost connection is retried every `STREAM_RETRY_MS`, forever. The pill in the page head says `Live` or `Reconnecting`. A dead feed is never mistaken for a quiet one. The Timeline's own staleness marking uses the same heartbeat.

The endpoints behind the tabs stay. Each serves the same payload a push is built from, for the Copy JSON control and for an operator with `curl`. The page does not read them to stay current.

## GitHub calls behind a pushed tab

A pushed tab is built many times a minute under traffic. Two tabs call GitHub in a build, so `ttlValue` holds those answers for a short time:

- the Rate limit tab's per-installation poll, for `liveRatesTTL`.
- the Webhooks tab's App subscription list, for `subscribedTTL`.

Without the hold, the rate-limit poll feeds itself: its own response headers raise the Rate limit topic, and that builds the tab again.

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

- `live_test.go` covers the stream, the request cursor and the topic mapping.
- `live_feeds_test.go` covers each tab's pushes: the first whole payload, a push on change, silence on no change and the delivery delta. This also covers the Timeline cursor and the admin gate.
- `TestRequestLogModel` runs the built log model under node, including its cost with a full buffer.
- `testdata/dashboardcheck.test.ts` (`just test-dashboard`) starts the real server and a real Chromium. It requires that a request reaches the page at once, and that a flood loses no event and blocks no frame. It requires that a dead server is reported. It also requires that no tab makes a request, idle or under traffic, after its set-up.
