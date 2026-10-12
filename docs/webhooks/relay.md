# Webhook relay

The GitHub App has one webhook URL, and it points at this service. `WEBHOOK_RELAY_URLS` names more consumers of the same deliveries. As a result, a second mirror stays current without a change to the reverse proxy. The code is `internal/relay`.

## What is forwarded

- Only a delivery whose `X-Hub-Signature-256` verified. A rejected delivery never leaves the service.
- Every verified delivery, whatever this service's own dispatch outcome was. The relay queues it before dispatch.
- The body, byte-same, because the signature covers those exact bytes.
- GitHub's own headers and nothing else: `X-GitHub-Event`, `X-GitHub-Delivery`, `X-GitHub-Hook-ID`, `X-GitHub-Hook-Installation-Target-ID`, `X-GitHub-Hook-Installation-Target-Type`, `X-Hub-Signature`, `X-Hub-Signature-256`, `Content-Type`, `User-Agent`.

The target holds the same webhook secret and verifies the signature itself.

## Delivery

The answer to GitHub never waits on a target. Each target has its own bounded in-memory queue and one worker, which sends deliveries in arrival order.

| Target answer | Result |
| --- | --- |
| 2xx | delivered |
| 4xx other than 408 or 429 | permanent: logged at error level with the delivery id, counted, not retried |
| anything else, or no answer | retried on a fixed interval, until a 2xx or a permanent 4xx |

The retry interval never grows, and there is no try cap. A redirect is not followed and counts as a retryable failure, because it means the target URL is incorrect.

A full queue drops the delivery. The relay logs it at error level with the delivery id and the target, and counts it.

A retry after a timeout can deliver the same delivery twice. A target dedupes on `X-GitHub-Delivery` or orders by the payload's own clock.

| Default | Value |
| --- | --- |
| queue size per target | `1000` |
| retry interval | `10s` |
| attempt timeout | `10s` |

## Limits

- **The queue is memory-only.** A start loses every queued delivery. At shutdown the relay drains for up to 30s, then logs each target's undelivered count at error level.
- **Nothing recovers a lost relay delivery.** GitHub's failure log records this service's answer, not the relay's, so a target's gap replayer never sees the loss. The counters below are the only record.

## Visibility

- Every try is on the Timeline, on the `⇒ relay` lane. The client is built through `internal/httpobs`.
- The admin Webhooks tab shows each target: queued, in flight, delivered, retried, permanently failed, dropped, and the last error. The `relay` field of `/api/webhooks` carries the same data.

## Configuration

`WEBHOOK_RELAY_URLS` is a comma-separated list of absolute http or https URLs. An entry that is empty, not absolute, of another scheme, or listed twice fails startup with a message that names it. Unset or empty relays nothing, and startup logs one info line that says so.
