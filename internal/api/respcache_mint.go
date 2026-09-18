package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// ---- POST /app/installations/{id}/access_tokens ----

// cachedInstallationToken caches installation-token mints. The route sits
// OUTSIDE requireAuth: its Authorization bearer is a GitHub App JWT (which
// cannot resolve GET /user), so the handler verifies it itself via
// VerifyAppIdentity — the same unforgeable check the X-Mirror-Identity header
// uses — and partitions by the verified app id. Cache key: (app id,
// installation id, SHA- of the canonicalized request body) — an empty body
// and a permissions/repositories subset mint DIFFERENT tokens. A caller whose
// JWT does not verify is forwarded to GitHub unchanged, uncached.
func (h *handlers) cachedInstallationToken(w http.ResponseWriter, r *http.Request) {
	jwt := bearerToken(r)
	if jwt == "" {
		http.Error(w, "unauthorized: missing Authorization header", http.StatusUnauthorized)
		return
	}
	reqBody, err := io.ReadAll(io.LimitReader(r.Body, maxMintBodyBytes+1))
	if err != nil || len(reqBody) > maxMintBodyBytes {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	restoreBody := func() {
		r.Body = io.NopCloser(bytes.NewReader(reqBody))
		r.ContentLength = int64(len(reqBody))
	}

	ident, err := h.gh.VerifyAppIdentity(r.Context(), jwt)
	if err != nil {
		// Not a verifiable App JWT: forward unchanged, let GitHub decide.
		restoreBody()
		h.passthrough(w, r, PassIdentity)
		return
	}
	actorKey := fmt.Sprintf("app:%d", ident.ID)
	ctx := actor.WithActor(r.Context(), actorKey)
	if ident.Slug != "" {
		ctx = actor.WithName(ctx, ident.Slug)
	}
	// Outside requireAuth, so record the identity here for the dashboard's app:<id> label.
	if h.recordIdentity != nil {
		h.recordIdentity(ctx, actorKey, ident.Slug)
	}
	who := callerIdent{Key: actorKey, Name: ident.Slug}
	installID := chi.URLParam(r, "id")
	bodyHash := canonicalBodyHash(reqBody)

	now := time.Now()
	if t, ok, err := h.store.GetCachedInstallToken(ctx, actorKey, installID, bodyHash, now); err == nil && ok {
		h.reqlog.observeAs(r, who, DispHit, 0)
		h.serveInstallToken(w, t, true)
		return
	} else if err != nil {
		slog.Warn("install token cache read failed", "installation", installID, "error", err)
	}

	resp, respBody, overflow, err := h.fetchUpstream(r, reqBody)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	t, serveUntil, absorbed := absorbInstallToken(installID, bodyHash, resp.StatusCode, respBody, now)
	if overflow || !absorbed {
		h.replayUnstored(w, r, resp, respBody)
		return
	}
	if err := h.store.PutCachedInstallToken(ctx, actorKey, t, now, serveUntil); err != nil {
		slog.Warn("install token cache write failed", "installation", installID, "error", err)
	}
	h.reqlog.observeAs(r, who, DispMiss, resp.StatusCode)
	h.serveInstallToken(w, t, false)
}

// mintTokenJSON is the trimmed rebuild of a token-mint. GitHub's response
// has no URL fields, but it can carry a huge `repositories` array (full repo
// objects, urls and all) — dropped; output is exactly these state fields.
type mintTokenJSON struct {
	Token               string          `json:"token"`
	ExpiresAt           string          `json:"expires_at"`
	Permissions         json.RawMessage `json:"permissions,omitempty"`
	RepositorySelection string          `json:"repository_selection,omitempty"`
}

func (h *handlers) serveInstallToken(w http.ResponseWriter, t ghdata.CachedInstallToken, hit bool) {
	out := mintTokenJSON{Token: t.Token, ExpiresAt: t.TokenExpiresAt, RepositorySelection: t.RepositorySelection}
	if t.Permissions != "" {
		out.Permissions = json.RawMessage(t.Permissions)
	}
	body, err := marshalTrimmed(out)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeRebuilt(w, http.StatusCreated, body, hit)
}

// absorbInstallToken parses a mint response into cacheable state. Only a
// whose expires_at parses AND leaves usable lifetime past the safety buffer
// is absorbed — a token about to expire is served but never stored, so a
// cached mint always has at least mintExpiryBuffer of validity left.
func absorbInstallToken(installID, bodyHash string, status int, body []byte, now time.Time) (ghdata.CachedInstallToken, time.Time, bool) {
	if status != http.StatusCreated {
		return ghdata.CachedInstallToken{}, time.Time{}, false
	}
	var m struct {
		Token               string          `json:"token"`
		ExpiresAt           string          `json:"expires_at"`
		Permissions         json.RawMessage `json:"permissions"`
		RepositorySelection string          `json:"repository_selection"`
	}
	if err := json.Unmarshal(body, &m); err != nil || m.Token == "" || m.ExpiresAt == "" {
		return ghdata.CachedInstallToken{}, time.Time{}, false
	}
	exp, err := time.Parse(time.RFC3339, m.ExpiresAt)
	if err != nil {
		return ghdata.CachedInstallToken{}, time.Time{}, false
	}
	serveUntil := exp.Add(-mintExpiryBuffer)
	if !serveUntil.After(now) {
		return ghdata.CachedInstallToken{}, time.Time{}, false
	}
	return ghdata.CachedInstallToken{
		InstallationID: installID, BodyHash: bodyHash,
		Token: m.Token, TokenExpiresAt: m.ExpiresAt,
		Permissions: string(m.Permissions), RepositorySelection: m.RepositorySelection,
	}, serveUntil, true
}

// canonicalBodyHash re-marshals JSON with sorted keys, so equivalent bodies share a cache key.
func canonicalBodyHash(body []byte) string {
	canon := bytes.TrimSpace(body)
	if len(canon) > 0 {
		var v interface{}
		if err := json.Unmarshal(canon, &v); err == nil {
			if remarshaled, err := json.Marshal(v); err == nil {
				canon = remarshaled
			}
		}
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// ---- shared plumbing ----

// contentsResourceKey is the deny-cache resource key for contents read.
func contentsResourceKey(owner, repo, path, ref string) string {
	return owner + "/" + repo + "/" + path + "?ref=" + ref
}

// invalidateMintOnAuthFailure drops a mint whose grants went stale, per docs/cache/rest-routes.md.
func invalidateMintOnAuthFailure(ctx context.Context, store *ghdata.Store, tok string, resp *http.Response) {
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return
	}
	if !strings.HasPrefix(tok, "ghs_") {
		return
	}
	if rateLimitShaped(resp) {
		return
	}
	if err := store.InvalidateInstallTokenByToken(ctx, tok); err != nil {
		slog.Warn("mint invalidation on upstream auth failure failed", "status", resp.StatusCode, "error", err)
	}
}

// rateLimitShaped reports whether a refusal is rate limiting rather than a permission verdict.
func rateLimitShaped(resp *http.Response) bool {
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	return resp.Header.Get("X-Ratelimit-Remaining") == "0"
}
