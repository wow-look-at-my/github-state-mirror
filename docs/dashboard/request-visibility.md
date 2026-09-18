# Every request this service sends is on the dashboard

A request nobody can see is a request nobody can account for. It spends
rate-limit budget the operator is trying to explain, it can be slow, it can
fail, and none of that reaches the chart they are looking at to answer "what
is this thing doing right now?".

This is an operator ruling, not a preference (2026-07-15, restated
2026-08-10): **everything the mirror exchanges is on the Timeline chart, and a
gap is a bug.** The first cut of the chart scoped out ghclient's background
traffic as "deliberately not recorded" and was reverted for it. Never
reintroduce an observability exclusion.

## Why call-site instrumentation is not enough

Recording at the call site covers the calls you thought of. The ones that go
missing are the ones nobody thought of — including every call site added
later, by someone who did not know the rule existed.

The mirror had a live example. `internal/auth` built its own `http.Client` for
the OAuth handshake, so **every dashboard sign-in made two real GitHub calls
that appeared nowhere** — on the very page they were serving. Nothing was
wrong with the code that made them; nobody had written the recording line, and
nothing could have told them to.

So the property is held at the **client**, not the call. An outbound HTTP
client either reports what it sends, from its transport, or it is a hole.
`internal/httpobs` is the wrapper; four of the six senders below use it or an
equivalent transport hook.

## The complete inventory

| Sender | How it is observed | Disposition |
|---|---|---|
| `internal/ghclient` (all REST + GraphQL, token mints, delivery replays) | `SetExchangeObserver` wraps the transport | `internal` |
| API layer's own client (cached-route miss fetches, reveal probes) | `httpobs` transport; the call site puts the kind in the request context | `upstream`, `probe` |
| Passthrough `ReverseProxy` (including debounced batches) | `httpobs` transport on `ReverseProxy.Transport` | `upstream` |
| `internal/auth` (sign-in: token exchange + `GET /user`) | `httpobs` transport | `login` |
| `relayGitHubLogin` (github.com login relays for browser clients) | one call site, records itself | `relay` |
| `internal/notify` (subscriber notifications) | the notifier records each attempt with its attempt number and terminal flag — which a transport cannot know | `⇒ notify` lane |

Inbound traffic is charted separately by `requestLog.observe*` end-to-end from
the router's receipt stamp, and webhook deliveries by the handler. A
passthrough therefore produces **two** events: the inbound request the mirror
served, and the call it made to serve it. That is deliberate — under
debouncing the two counts stop matching, which is exactly the thing worth
seeing (many inbound bars, one call to GitHub inside them).

## The build gate

`internal/guards.TestEveryOutboundClientIsObserved` walks every non-test Go
file for anything that can originate a request — an `http.Client` literal, an
`httputil.ReverseProxy` literal, `http.DefaultClient` and the package-level
`http.Get`/`Post`/`PostForm`/`Head` — and requires each to be declared in
`observedClients` with the mechanism that makes it visible.

- An undeclared sender **fails the build**. Verified by adding one and
  watching it go red, not assumed.
- The declaration carries a **count** per file. Keying by file alone would let
  a second client appear in a listed file and inherit a sentence written about
  a different one.
- A declaration whose file no longer sends anything also fails: a stale
  sentence is one the next reader will believe.
- The package-level senders can never be excused — `http.DefaultClient` has no
  transport to wrap.

Test files are skipped: they talk to `httptest` servers, reach no real
network, and are not what an operator is watching.

## The one uncharted surface, stated openly

The dashboard's **own** UI endpoints — `/api/*`, the login pages, the assets —
are not on the chart. The chart polls `/api/timeline`, so charting it would
fill the chart with the act of looking at it.

This covers inbound requests to the dashboard only. The sign-in flow's
**outbound** calls to GitHub are charted (`login`), because they spend the
same budget as everything else and happen once rather than on every poll.

If the operator wants the UI endpoints too, add them. Do not argue.
