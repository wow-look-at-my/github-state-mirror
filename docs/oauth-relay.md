# OAuth relay (github.com login endpoints)

## The contract

`POST /login/oauth/access_token` and `POST /login/device/code`
(`internal/api/oauth.go`, one shared `relayGitHubLogin` core) relay the two
browser-blocked `github.com` login endpoints (NOT `api.github.com`): the OAuth
code-for-token exchange and the RFC 8628 device-authorization start, each
returned with the mirror's CORS headers so a purely client-side app can
complete login (GitHub's login endpoints send no CORS headers). The device
flow's polling leg (`grant_type`
`urn:ietf:params:oauth:grant-type:device_code`) goes through the access_token
route unchanged — the body is opaque bytes to the relay. Both are registered
outside `requireAuth` (the body is the credential — an OAuth `client_secret`,
or just a public `client_id` for the device flow; no bearer token) and forward
the body + content-negotiation headers to fixed URLs
(`githubOAuthTokenURL`/`githubDeviceCodeURL`, vars so tests can point them at a
fake). They do NOT copy GitHub's `Access-Control-*` (corsMiddleware is the
single CORS authority, so no duplicate ACAO). This is why repo-nightmare's
`TOKEN_PROXY` points here instead of a standalone CORS proxy.

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

## The relay holds the client secret

A browser-only app cannot hold a client secret: whatever it ships, every
visitor downloads. GitHub Apps support no PKCE, so the authorization-code
exchange still needs a secret, and this relay is the only server standing in
that exchange. `OAUTH_RELAY_SECRETS` therefore maps a client id to its secret
(`<client_id>=<client_secret>`, comma-separated), and `addClientSecret` sets
`client_secret` on a form-encoded body whose `client_id` is configured. What
the caller sent under that name is replaced, never trusted.

A body the relay adds nothing to is forwarded as it arrived: anything that is
not a form, an app nothing is configured for, and the device flow, which
authenticates on the public client id alone. When an authorization-code
exchange names an app with no configured secret, GitHub answers
`incorrect_client_credentials` — the browser shows that, and the relay logs a
warning naming the client id, so the failure is loud at both ends. A malformed
`OAUTH_RELAY_SECRETS` entry fails startup rather than dropping a pair and
turning every sign-in for that app into the same error.

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
