# The payload audit test

GitHub hands us the state it already computed, in the delivery. Suppose we subscribe to an event and then answer it with a cache flush, or with nothing. That costs a request, a rate-limit slot and a staleness window for information we already held. That is this service's signature defect. As a result, it is a build failure rather than something a reviewer has to notice.

For EVERY event type the dispatcher handles, exactly one of:

- the handler provably reads the delivery body, or
- `docs/webhooks/payload-unused/<event>.md` explains, in detail, why not.

Checked BOTH ways. An event whose handler DOES read its payload may not also carry an exception doc, so an excuse cannot outlive its reason. The day someone teaches a handler to use its delivery, this test fails until the document is deleted in that same change.

This is an AST walk over this package, not a grep. The handlers delegate: `onPullRequestReview` is a one-line call to `applyPRPayload`. Anything shallower thus reports false violations. A guard that cries wolf is worse than no guard.

## Two scoping rules, both deliberate

- `handle()` calls `absorbRepoFromPayload` for every delivery, which parses the repository object. It is NOT credited here. That is the envelope every payload carries, absorbed the same way whatever the event. Crediting it will pass every handler and make this test decorative. The question is whether the handler uses ITS OWN event's content.
- The `default:` arm (an event type we do not handle) is out of scope. This audits the events we USE. An event that reaches default is not being used wastefully. It is not being used at all.
