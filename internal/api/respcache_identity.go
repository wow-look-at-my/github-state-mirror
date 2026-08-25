package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Implements the identity/self routes on "identical-or-passthrough": the
// upstream body is cached VERBATIM, never trimmed. See docs/cache/rest-routes.md.

// ---- GET /app (JWT-verified, outside requireAuth like the installation-mint
// routes: an installation token cannot resolve this endpoint at all) ----

func (h *handlers) cachedApp(w http.ResponseWriter, r *http.Request) {
	jwt := bearerToken(r)
	if jwt == "" {
		h.passthrough(w, r, PassIdentity)
		return
	}
	ident, err := h.gh.VerifyAppIdentity(r.Context(), jwt)
	if err != nil {
		h.passthrough(w, r, PassIdentity)
		return
	}
	if !acceptsDefaultJSON(r) || r.URL.RawQuery != "" {
		h.passthrough(w, r, shapeReason(r, true))
		return
	}
	subjectKey := "app:" + strconv.FormatInt(ident.ID, 10)
	ctx := actor.WithActor(r.Context(), subjectKey)
	if ident.Slug != "" {
		ctx = actor.WithName(ctx, ident.Slug)
	}
	if h.recordIdentity != nil {
		h.recordIdentity(ctx, subjectKey, ident.Slug)
	}
	h.serveIdentity(w, r.WithContext(ctx), subjectKey, ghdata.IdentityKindApp, absorbVerbatimObject)
}

// ---- GET /user (inside requireAuth: the requesting token's own profile) ----

func (h *handlers) cachedUser(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" || !acceptsDefaultJSON(r) || r.URL.RawQuery != "" {
		h.passthrough(w, r, shapeReason(r, token != ""))
		return
	}
	fp := ghclient.Fingerprint(token)
	h.serveIdentity(w, r, fp, ghdata.IdentityKindUser, absorbVerbatimObject)
}

// ---- GET /user/orgs (inside requireAuth: the requesting token's own org
// memberships) ----

func (h *handlers) cachedUserOrgs(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" || !acceptsDefaultJSON(r) || r.URL.RawQuery != "" {
		h.passthrough(w, r, shapeReason(r, token != ""))
		return
	}
	fp := ghclient.Fingerprint(token)
	h.serveIdentity(w, r, fp, ghdata.IdentityKindUserOrgs, absorbVerbatimArray)
}

// identityAbsorber reports whether an upstream identity response is cacheable verbatim.
type identityAbsorber func(status int, body []byte) (string, bool)

// absorbVerbatimObject accepts a 200 JSON object with a positive numeric id; the stored bytes are the original body.
func absorbVerbatimObject(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	var probe struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.ID <= 0 {
		return "", false
	}
	return string(body), true
}

// absorbVerbatimArray accepts a 200 JSON array; an empty array is a valid cacheable answer.
func absorbVerbatimArray(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	var probe []json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", false
	}
	return string(body), true
}

// serveIdentity is the shared miss/absorb/hit flow for all three identity
// routes: they differ only in their subject key, their cache kind, and their
// absorb function.
func (h *handlers) serveIdentity(w http.ResponseWriter, r *http.Request, subjectKey, kind string, absorb identityAbsorber) {
	now := time.Now()
	if doc, ok, err := h.store.GetCachedIdentity(r.Context(), subjectKey, kind, now); err != nil {
		slog.Warn("identity cache read failed", "subject", subjectKey, "kind", kind, "error", err)
	} else if ok {
		h.reqlog.observe(r, DispHit)
		writeRebuilt(w, http.StatusOK, []byte(doc), true)
		return
	}

	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	doc, absorbed := absorb(resp.StatusCode, body)
	if overflow || !absorbed {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedIdentity(r.Context(), subjectKey, kind, doc, now, ghdata.IdentityCacheTTL); err != nil {
		slog.Warn("identity cache write failed", "subject", subjectKey, "kind", kind, "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}
