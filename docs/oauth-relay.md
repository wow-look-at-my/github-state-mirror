# OAuth relay (github.com login endpoints)

`oauthAccessToken` relays a GitHub OAuth "exchange code for token" POST to
github.com and returns GitHub's response with the mirror's CORS headers.

A fully client-side app (e.g. the repo-nightmare PR viewer) cannot POST to
github.com/login/oauth/access_token directly: that endpoint sends no CORS
headers, so the browser blocks the JS from reading the response and the login
silently fails. The mirror already attaches correct CORS (corsMiddleware), so
it stands in as the relay — removing the need for a separate CORS proxy.

This is deliberately NOT the api.github.com passthrough: the OAuth endpoints
live on github.com, and the exchange authenticates with the client_id/secret
in the body (no bearer token), so it is registered outside requireAuth and
targets a fixed github.com URL rather than proxying an arbitrary path.

The device flow's polling leg (grant_type
"urn:ietf:params:oauth:grant-type:device_code") goes through this same
endpoint unchanged — the body is opaque bytes to the relay.

`oauthDeviceCode` relays a GitHub device authorization request (RFC 8628, the
"start a device flow" POST that mints a user_code) to github.com and returns
GitHub's response with the mirror's CORS headers.

Same story as oauthAccessToken: github.com/login/device/code sends no CORS
headers, so a browser-only client can never start a device sign-in on its
own. The request carries only the app's public client_id + scope (no secret,
no bearer token), so it too sits outside requireAuth and targets a fixed
github.com URL — not the api.github.com passthrough. The subsequent polling
leg reuses the access-token relay above.
