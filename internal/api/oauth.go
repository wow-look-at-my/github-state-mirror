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

// oauthRelayClient performs the server-side leg of the OAuth token exchange.
var oauthRelayClient = &http.Client{Timeout: 15 * time.Second}

// maxOAuthBytes caps the request and response bodies of the token exchange.
// These payloads are tiny (a few form fields in, a token out); this only guards
// memory.
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
func (h *handlers) oauthAccessToken(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOAuthBytes+1))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(body) > maxOAuthBytes {
		http.Error(w, "request entity too large", http.StatusRequestEntityTooLarge)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, githubOAuthTokenURL, bytes.NewReader(body))
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

	resp, err := oauthRelayClient.Do(req)
	if err != nil {
		slog.Warn("oauth token exchange failed", "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Pass GitHub's status, content type, and body through verbatim so the
	// client can parse the token exactly as if it had called GitHub directly.
	// CORS headers come from corsMiddleware; GitHub's own Access-Control-* are
	// intentionally not copied, so there is never a duplicate ACAO.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxOAuthBytes))
}
