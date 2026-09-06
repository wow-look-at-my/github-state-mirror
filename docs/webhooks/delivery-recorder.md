# The delivery recorder (`internal/webhook`)

Extracted from CLAUDE.md, where the whole rule no longer fits.

The webhook handler dispatches SYNCHRONOUSLY. It returns a per-delivery disposition in the response, so GitHub's own delivery record states whether the cache moved.

## The ingest notifier

After each dispatch the handler hands the outcome to any `IngestNotifier`. The parameter is variadic, optional and nil-safe, and `internal/notify` implements it. The call is non-blocking by contract, so the response to GitHub never waits on subscribers. See docs/notifications.md.

## What the recorder observes

An optional `DeliveryRecorder` observes EVERY delivery attempt, with its real measured duration, for the dashboard's Timeline chart. A nil recorder keeps it inert. `internal/api` adapts it onto `reqtimeline`.

It records a verified delivery after dispatch. It records a rejected delivery at the moment of refusal, under a disposition only the recorder ever sees.

- `unverified` — a bad or missing signature, or an unset secret.
- `unparseable` — verified, but no event type.
- `rejected` — the wrong method, or an unreadable body.

## Rejection metadata is untrusted

Anyone can post to the webhook endpoint, so a rejected delivery's metadata is attacker-controlled. The recorder puts every rejection on one fixed lane and clamps the detail. GitHub-facing responses are unchanged by any of this.
