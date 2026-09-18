package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
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
