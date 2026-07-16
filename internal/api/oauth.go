package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// githubOAuthTokenURL is the GitHub endpoint that exchanges an OAuth
// authorization code for a user access token. A var (not const) so tests can
// point it at a fake server; never reassigned in production.
var githubOAuthTokenURL = "https://github.com/login/oauth/access_token"

// githubDeviceCodeURL is the GitHub endpoint that starts an RFC 8628 device
// authorization flow (the "enter this code at github.com/login/device" sign-in
// the gh CLI uses). A var (not const) so tests can point it at a fake server;
// never reassigned in production.
var githubDeviceCodeURL = "https://github.com/login/device/code"

// oauthRelayClient performs the server-side leg of the github.com login relays.
var oauthRelayClient = &http.Client{Timeout: 15 * time.Second}

// maxOAuthBytes caps the request and response bodies of the login relays.
// These payloads are tiny (a few form fields in, a token or device code out);
// this only guards memory.
const maxOAuthBytes = 64 << 10 // 64 KiB

// oauthAccessToken relays a GitHub OAuth "exchange code for token" POST to
// github.com and returns GitHub's response with the mirror's CORS headers.
//
// A fully client-side app (e.g. the repo-nightmare PR viewer) cannot POST to
// github.com/login/oauth/access_token directly: that endpoint sends no CORS
// headers, so the browser blocks the JS from reading the response and the login
// silently fails. The mirror already attaches correct CORS (corsMiddleware), so
// it stands in as the relay — removing the need for a separate CORS proxy.
//
// This is deliberately NOT the api.github.com passthrough: the OAuth endpoints
// live on github.com, and the exchange authenticates with the client_id/secret
// in the body (no bearer token), so it is registered outside requireAuth and
// targets a fixed github.com URL rather than proxying an arbitrary path.
//
// The device flow's polling leg (grant_type
// "urn:ietf:params:oauth:grant-type:device_code") goes through this same
// endpoint unchanged — the body is opaque bytes to the relay.
func (h *handlers) oauthAccessToken(w http.ResponseWriter, r *http.Request) {
	h.relayGitHubLogin(w, r, githubOAuthTokenURL)
}

// oauthDeviceCode relays a GitHub device authorization request (RFC 8628, the
// "start a device flow" POST that mints a user_code) to github.com and returns
// GitHub's response with the mirror's CORS headers.
//
// Same story as oauthAccessToken: github.com/login/device/code sends no CORS
// headers, so a browser-only client can never start a device sign-in on its
// own. The request carries only the app's public client_id + scope (no secret,
// no bearer token), so it too sits outside requireAuth and targets a fixed
// github.com URL — not the api.github.com passthrough. The subsequent polling
// leg reuses the access-token relay above.
func (h *handlers) oauthDeviceCode(w http.ResponseWriter, r *http.Request) {
	h.relayGitHubLogin(w, r, githubDeviceCodeURL)
}

// relayGitHubLogin forwards a login POST body verbatim to the given github.com
// endpoint and passes the response back untouched — the shared core of the
// token-exchange and device-code relays. Only the content-negotiation headers
// travel upstream; CORS on the way back is corsMiddleware's alone. Each
// upstream call is timed onto the Timeline chart (disposition "relay") under
// the mirror's own fixed relay path as the lane — these carry no bearer
// token, so the actor is "anonymous".
func (h *handlers) relayGitHubLogin(w http.ResponseWriter, r *http.Request, upstreamURL string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOAuthBytes+1))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(body) > maxOAuthBytes {
		http.Error(w, "request entity too large", http.StatusRequestEntityTooLarge)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Forward only the content-negotiation headers; the client_id/secret/code
	// travel in the body. Arbitrary client headers are intentionally not
	// forwarded to GitHub.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if ac := r.Header.Get("Accept"); ac != "" {
		req.Header.Set("Accept", ac)
	}

	start := time.Now()
	resp, err := oauthRelayClient.Do(req)
	if err != nil {
		h.timeline.RecordRequest(start, time.Since(start), http.MethodPost, r.URL.Path, 0, DispError, "anonymous", "")
		slog.Warn("github login relay failed", "url", upstreamURL, "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	h.timeline.RecordRequest(start, time.Since(start), http.MethodPost, r.URL.Path, resp.StatusCode, dispRelay, "anonymous", "")
	defer resp.Body.Close()

	// Pass GitHub's status, content type, and body through verbatim so the
	// client can parse the answer exactly as if it had called GitHub directly.
	// CORS headers come from corsMiddleware; GitHub's own Access-Control-* are
	// intentionally not copied, so there is never a duplicate ACAO.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxOAuthBytes))
}
