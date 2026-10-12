# API package test harness

## schemaTemplate (internal/api/api_test.go)

schemaTemplate is a blank cache DB built ONCE per test binary and copied for every test that needs one. Creating one costs the CREATE TABLE + CREATE INDEX statements of schema.sql and the fsync that lands them. This package opens a fresh DB per test across several hundred of them, against go-toolchain's hard 30s per-binary timeout. A CI run has already died inside exactly that exec. Copying the finished file costs a fraction of it, and database.Open then takes its existing-file path -- same pragmas, same fingerprint check, same DB.

## newTestStackWithGitHub (internal/api/api_test.go)

newTestStackWithGitHub is like newTestStack. However, newTestStackWithGitHub lets the caller supply the fake upstream GitHub handler, and returns its URL -- used by passthrough tests that need to observe forwarded requests. This is requireAuth resolves the bearer token against this same handler. As a result, it must answer GET /user with a login.

## testStack.debouncer (internal/api/api_test.go)

debouncer is nil unless the test asked for passthrough coalescing via newFullTestStackDebounced. Nil is the right default: a real hold window will add its full delay to every passthrough in every other test.

## countingUpstream (internal/api/debounce_test.go)

countingUpstream is a stand-in for the GitHub passthrough proxy: it records every call that reached "upstream" and echoes an identifying body. As a result, a test can prove how many round trips N inbound requests cost.

## TestDebounce_IneligibleRequestsForwardImmediately

The exclusions are not uncoalesced. They are UNDELAYED — an ineligible request must not pay the window. Writes are excluded because same POSTs are intents. Conditional and Range reads because their answer depends on caller state the key cannot capture (a 304 against someone else's etag is a incorrect answer).

## TestDebounce_ThroughRouterRecordsStats

The window must outlast the spread between the first and last waiter ARRIVING at the debouncer, or the batch splits. Only n-2 calls are saved. Through the real router that spread is real work, not scheduling noise: an unseen token costs requireAuth a trip of the GET /user round. The reveal layer a repo probe plus its grant write, and goroutines race each other for the same SQLite writer to do it. So the test warms both before firing, and keeps a window wide enough to absorb what a loaded runner still adds. This assertion failed in CI at 100ms.

## workflowRunsUpstream.runs (internal/api/respcache_actionsruns_test.go)

total_count deliberately EXCEEDS the page (GitHub's total matching count vs the page length -- exactly what pr-minder's per_page=1 probe relies on), and the sha echoes the request's filter so distinct shas produce distinguishable docs. A request that names no sha (the repo-wide listing shape) still gets runs carrying a real one, exactly as GitHub answers it.

## TestProxy_DeduplicatesCORS (internal/api/proxy_test.go)

Expose-Headers is the one CORS header the proxy deliberately does NOT strip. As a result, a passthrough exposes the UNION of GitHub's list and the mirror's own (repeated list-valued header fields combine, per RFC 9110 -- so `Link` stays readable and the mirror's X-GSM-* join it). Only the Allow-* headers must be singular, since a browser rejects a duplicated Allow-Origin outright.
