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
	"github.com/wow-look-at-my/go-containers/set"
)

// This file implements the cached personalized repo listing:
//
//	GET /user/repos
//
// Distinct from the org-repos GraphQL tier-1 query: this REST listing spans
// every repo the token can see, across every owner, sorted however the
// caller asked. Keyed by the bearer's fingerprint + the modeled query shape,
// cached VERBATIM (see respcache_identity.go's file header). No webhook
// interaction; see ghdata/respcache_userrepos.go for why.

const (
	userReposDefaultPerPage = 30
	userReposMaxCachedPage  = 10
)

var userReposAllowedSorts = set.Of("", "created", "updated", "pushed", "full_name")

func (h *handlers) cachedUserRepos(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		h.passthrough(w, r, PassIdentity)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	sort, perPage, page, ok := parseUserReposShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	fp := ghclient.Fingerprint(token)

	now := time.Now()
	if doc, ok, err := h.store.GetCachedUserRepos(r.Context(), fp, sort, perPage, page, now); err != nil {
		slog.Warn("user repos cache read failed", "error", err)
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
	if err := h.store.PutCachedUserRepos(r.Context(), fp, sort, perPage, page, string(body), now, ghdata.UserReposCacheTTL); err != nil {
		slog.Warn("user repos cache write failed", "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, body, false)
}

// parseUserReposShape reports the modeled shape: sort (one of GitHub's
// documented values, '' = default) + paging. Every other parameter
// (affiliation, visibility, type, direction, since, before) is deliberately
// unmodeled and passes through -- the surveyed caller (see the brief) sends
// only page/per_page/sort.
func parseUserReposShape(q url.Values) (sort string, perPage, page int64, ok bool) {
	perPage, page = userReposDefaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return "", 0, 0, false
		}
		switch key {
		case "sort":
			if !userReposAllowedSorts.Contains(vals[0]) {
				return "", 0, 0, false
			}
			sort = vals[0]
		case "per_page":
			n, err := strconv.ParseInt(vals[0], 10, 64)
			if err != nil || n < 1 || n > 100 {
				return "", 0, 0, false
			}
			perPage = n
		case "page":
			n, err := strconv.ParseInt(vals[0], 10, 64)
			if err != nil || n < 1 || n > userReposMaxCachedPage {
				return "", 0, 0, false
			}
			page = n
		default:
			return "", 0, 0, false
		}
	}
	return sort, perPage, page, true
}
