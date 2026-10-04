# The delivery recorder (`internal/webhook`)

Extracted from CLAUDE.md, where the whole rule no longer fits.

The webhook handler answers GitHub once the delivery is in the inbox, and the queue's worker applies it (docs/webhooks/dispatch.md, "Receipt and apply"). The applied disposition reaches the recorder, the `webhook_deliveries` log and the notifiers.

## The ingest notifier

After each apply the queue hands the outcome to any `IngestNotifier`. The parameter is variadic, optional and nil-safe, and `internal/notify` implements it. The call is non-blocking by contract, so the worker never waits on subscribers. See docs/notifications.md.

## What the recorder observes

An optional `DeliveryRecorder` observes EVERY delivery attempt, with its real measured duration, for the dashboard's Timeline chart. A nil recorder keeps it inert. `internal/api` adapts it onto `reqtimeline`.

It records a verified delivery after its apply, timed from receipt to applied. It records a `duplicate` and a delivery the inbox can not record at the moment of the answer. It records a rejected delivery at the moment of refusal, under a disposition only the recorder ever sees.

- `unverified` — a bad or missing signature, or an unset secret.
- `unparseable` — verified, but no event type.
- `rejected` — the wrong method, or an unreadable body.

## Rejection metadata is untrusted

Anyone can post to the webhook endpoint, so a rejected delivery's metadata is attacker-controlled. The recorder puts every rejection on one fixed lane and clamps the detail. GitHub-facing responses are unchanged by any of this.
