package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Cached GET /app/installations, JWT-verified, keyed by app id.
const (
	appInstallationsDefaultPerPage = 30
	appInstallationsMaxCachedPage  = 10
)

func (h *handlers) cachedAppInstallations(w http.ResponseWriter, r *http.Request) {
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
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	perPage, page, ok := parseAppInstallationsShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	appKey := "app:" + strconv.FormatInt(ident.ID, 10)

	now := time.Now()
	if doc, ok, err := h.store.GetCachedAppInstallations(r.Context(), appKey, perPage, page, now); err != nil {
		slog.Warn("app installations cache read failed", "app", appKey, "error", err)
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

	if overflow || resp.StatusCode != http.StatusOK || !json.Valid(body) {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedAppInstallations(r.Context(), appKey, perPage, page, string(body), now, ghdata.AppInstallationsCacheTTL); err != nil {
		slog.Warn("app installations cache write failed", "app", appKey, "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, body, false)
}

// parseAppInstallationsShape reports the paging shape, the hooks-route
// pattern.
func parseAppInstallationsShape(q url.Values) (perPage, page int64, ok bool) {
	perPage, page = appInstallationsDefaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return 0, 0, false
		}
		n, err := strconv.ParseInt(vals[0], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		switch key {
		case "per_page":
			if n < 1 || n > 100 {
				return 0, 0, false
			}
			perPage = n
		case "page":
			if n < 1 || n > appInstallationsMaxCachedPage {
				return 0, 0, false
			}
			page = n
		default:
			return 0, 0, false
		}
	}
	return perPage, page, true
}
