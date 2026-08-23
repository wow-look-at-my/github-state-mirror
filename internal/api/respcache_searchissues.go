package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file implements the cached issue/PR search:
//
//	GET /search/issues
//
// The documented exception to webhook-driven cache maintenance -- see
// ghdata/respcache_searchissues.go's file header and
// docs/cache/uncacheable-routes.md. Keyed by the bearer's fingerprint (search
// results are permission-scoped: GitHub filters hits to what the caller's
// token can see, so a row shared across credentials would leak one caller's
// results to another) plus the modeled query shape (q, per_page, page).
// Cached VERBATIM.

const (
	searchIssuesDefaultPerPage = 30
	searchIssuesMaxCachedPage  = 10
)

func (h *handlers) cachedSearchIssues(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		h.passthrough(w, r, PassIdentity)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	q, perPage, page, ok := parseSearchIssuesShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	fp := ghclient.Fingerprint(token)
	queryKey := ghdata.SearchIssuesQueryKey(q, perPage, page)

	now := time.Now()
	if doc, ok, err := h.store.GetCachedSearchIssues(r.Context(), fp, queryKey, now); err != nil {
		slog.Warn("search issues cache read failed", "error", err)
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
	if err := h.store.PutCachedSearchIssues(r.Context(), fp, queryKey, string(body), now, ghdata.SearchIssuesCacheTTL); err != nil {
		slog.Warn("search issues cache write failed", "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, body, false)
}

// parseSearchIssuesShape reports the modeled shape: q (required, unbounded
// text -- hashed into the cache key, never stored raw) + paging. sort and
// order are deliberately unmodeled and pass through -- the surveyed caller
// (see the brief) sends only q/page/per_page.
func parseSearchIssuesShape(qs url.Values) (q string, perPage, page int64, ok bool) {
	perPage, page = searchIssuesDefaultPerPage, 1
	for key, vals := range qs {
		if len(vals) != 1 {
			return "", 0, 0, false
		}
		switch key {
		case "q":
			if vals[0] == "" {
				return "", 0, 0, false
			}
			q = vals[0]
		case "per_page":
			n, err := strconv.ParseInt(vals[0], 10, 64)
			if err != nil || n < 1 || n > 100 {
				return "", 0, 0, false
			}
			perPage = n
		case "page":
			n, err := strconv.ParseInt(vals[0], 10, 64)
			if err != nil || n < 1 || n > searchIssuesMaxCachedPage {
				return "", 0, 0, false
			}
			page = n
		default:
			return "", 0, 0, false
		}
	}
	if q == "" {
		return "", 0, 0, false
	}
	return q, perPage, page, true
}
