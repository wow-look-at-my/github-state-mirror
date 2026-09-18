package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Cached org self-hosted-runners listing: GET /orgs/{org}/actions/runners
// see docs/cache/rest-routes.md

const (
	orgRunnersDefaultPerPage = 30
	orgRunnersMaxCachedPage  = 10
)

func (h *handlers) cachedOrgRunners(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		h.passthrough(w, r, PassIdentity)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	perPage, page, ok := parseOrgRunnersShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	org := ghdata.NormalizeRepoKey(chi.URLParam(r, "org"))
	fp := ghclient.Fingerprint(token)

	now := time.Now()
	if doc, ok, err := h.store.GetCachedOrgRunners(r.Context(), fp, org, perPage, page, now); err != nil {
		slog.Warn("org runners cache read failed", "org", org, "error", err)
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
		//  (not an admin) is deliberately not a cached verdict: a permission grant can change silently.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedOrgRunners(r.Context(), fp, org, perPage, page, string(body), now, ghdata.OrgRunnersCacheTTL); err != nil {
		slog.Warn("org runners cache write failed", "org", org, "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, body, false)
}

// parseOrgRunnersShape reports the paging shape, the hooks-route pattern.
func parseOrgRunnersShape(q url.Values) (perPage, page int64, ok bool) {
	perPage, page = orgRunnersDefaultPerPage, 1
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
			if n < 1 || n > orgRunnersMaxCachedPage {
				return 0, 0, false
			}
			page = n
		default:
			return 0, 0, false
		}
	}
	return perPage, page, true
}
