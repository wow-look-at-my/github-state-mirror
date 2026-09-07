package api

import (
	"bytes"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"time"
)

// var so tests can point it at a fake server.
var githubOAuthTokenURL = "https://github.com/login/oauth/access_token"

// var so tests can point it at a fake server.
var githubDeviceCodeURL = "https://github.com/login/device/code"

// oauthRelayClient performs the server-side leg of the github.com login relays.
var oauthRelayClient = &http.Client{Timeout: 15 * time.Second}

// caps the login relay request/response bodies.
const maxOAuthBytes = 64 << 10 //  KiB

// oauthAccessToken relays the OAuth code-for-token exchange to github.com,
// which sends no CORS headers of its own.
func (h *handlers) oauthAccessToken(w http.ResponseWriter, r *http.Request) {
	h.relayGitHubLogin(w, r, githubOAuthTokenURL)
}

// oauthDeviceCode relays the RFC device-authorization start, same story
// as oauthAccessToken.
func (h *handlers) oauthDeviceCode(w http.ResponseWriter, r *http.Request) {
	h.relayGitHubLogin(w, r, githubDeviceCodeURL)
}

// addClientSecret puts the configured client secret into a form-encoded login
// body. A browser-only app cannot carry a secret -- whatever it ships, every
// visitor has -- and GitHub Apps support no PKCE, so this relay is the only
// server in that exchange and the secret belongs here.
//
// The body is returned unchanged for anything that is not a form, for a client
// id nothing is configured for, and for the device flow, which authenticates
// on the public client id alone.
func (h *handlers) addClientSecret(body []byte, contentType string) []byte {
	if len(h.oauthRelaySecrets) == 0 {
		return body
	}
	if mt, _, err := mime.ParseMediaType(contentType); err != nil || mt != "application/x-www-form-urlencoded" {
		return body
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return body
	}
	clientID := form.Get("client_id")
	secret, ok := h.oauthRelaySecrets[clientID]
	if !ok {
		// GitHub answers incorrect_client_credentials and the browser shows it,
		// so this is the mirror's half of a failure the caller already sees.
		if needsClientSecret(form) {
			slog.Warn("oauth relay has no client secret for this app", "client_id", clientID)
		}
		return body
	}
	form.Set("client_secret", secret)
	return []byte(form.Encode())
}

// needsClientSecret reports whether an exchange authenticates the app itself.
// The authorization-code exchange does; the device flow does not.
func needsClientSecret(form url.Values) bool {
	if form.Get("device_code") != "" {
		return false
	}
	return form.Get("code") != ""
}

// relayGitHubLogin forwards a login POST body to the given github.com endpoint
// and passes the response back untouched — the shared core of the
// token-exchange and device-code relays. The body travels as it arrived except
// for the client secret addClientSecret adds. Only the content-negotiation headers
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

	body = h.addClientSecret(body, r.Header.Get("Content-Type"))

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Forward only content-negotiation headers; the client secret is in the body.
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

	// Pass GitHub's status, content type, and body through verbatim.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxOAuthBytes))
}
