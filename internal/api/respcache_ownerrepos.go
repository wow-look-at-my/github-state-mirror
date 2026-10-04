package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
	"github.com/wow-look-at-my/go-containers/set"
)

// The cached owner repo listings. Per-credential keying is self-gating, so
// neither takes a reveal check. see docs/cache/rest-routes.md

const (
	// GitHub's default per_page for both listings.
	ownerReposDefaultPerPage = 30

	// Caps modeled pages; deeper pagination passes through.
	ownerReposMaxCachedPage = 10
)

// The documented sort and direction values; anything else passes through.
var (
	ownerReposAllowedSorts      = set.Of("", "created", "updated", "pushed", "full_name")
	ownerReposAllowedDirections = set.Of("", "asc", "desc")
)

// cachedOwnerRepos serves an owner's repo listing, at the path spelling the
// scope names. The spellings answer differently for the same name, since the
// user listing of an organization omits the organization's own repos, so scope
// is part of the row key and never a synonym.
func (h *handlers) cachedOwnerRepos(scope, param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			h.passthrough(w, r, PassIdentity)
			return
		}
		if !acceptsDefaultJSON(r) {
			h.passthrough(w, r, PassAccept)
			return
		}
		sort, direction, perPage, page, ok := parseOwnerReposShape(r.URL.Query())
		if !ok {
			h.passthrough(w, r, PassQuery)
			return
		}
		owner := ghdata.NormalizeRepoKey(chi.URLParam(r, param))
		if owner == "" {
			h.passthrough(w, r, PassPath)
			return
		}
		key := ghdata.OwnerReposKey{
			TokenFP: ghclient.Fingerprint(token), Scope: scope, Owner: owner,
			Sort: sort, Direction: direction, PerPage: perPage, Page: page,
		}

		now := time.Now()
		if c, ok, err := h.store.GetCachedOwnerRepos(r.Context(), key, now); err != nil {
			slog.Warn("owner repos cache read failed", "scope", scope, "owner", owner, "error", err)
		} else if ok {
			h.reqlog.observe(r, DispHit)
			writeRebuilt(w, c.Status, []byte(c.Doc), true)
			return
		}

		resp, body, overflow, err := h.fetchUpstream(r, nil)
		if err != nil {
			h.upstreamError(w, r, err)
			return
		}
		defer resp.Body.Close()

		rows, c, absorbed := absorbOwnerRepos(resp.StatusCode, body)
		if overflow || !absorbed {
			// A refusal, a transient failure, any unmodeled shape: relayed verbatim, never stored.
			h.replayUnstored(w, r, resp, body)
			return
		}
		// Every listed repo is a fresh view of global truth, whoever asked for it.
		for _, row := range rows {
			if err := h.store.UpsertRepo(r.Context(), row); err != nil {
				slog.Warn("owner repos truth absorb failed", "scope", scope, "owner", owner, "repo", row.Name, "error", err)
				break
			}
		}
		if err := h.store.PutCachedOwnerRepos(r.Context(), key, c, now, ghdata.OwnerReposCacheTTL); err != nil {
			slog.Warn("owner repos cache write failed", "scope", scope, "owner", owner, "error", err)
		}
		h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
		writeRebuilt(w, c.Status, []byte(c.Doc), false)
	}
}

// parseOwnerReposShape reports the modeled shape: sort, direction and paging,
// of GitHub's documented values. The type filter is deliberately unmodeled and
// passes through; the surveyed callers (see the implementation brief) send
// page, per_page, sort and direction.
func parseOwnerReposShape(q url.Values) (sort, direction string, perPage, page int64, ok bool) {
	perPage, page = ownerReposDefaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return "", "", 0, 0, false
		}
		switch key {
		case "sort":
			if !ownerReposAllowedSorts.Contains(vals[0]) {
				return "", "", 0, 0, false
			}
			sort = vals[0]
		case "direction":
			if !ownerReposAllowedDirections.Contains(vals[0]) {
				return "", "", 0, 0, false
			}
			direction = vals[0]
		case "per_page":
			n, err := strconv.ParseInt(vals[0], 10, 64)
			if err != nil || n < 1 || n > 100 {
				return "", "", 0, 0, false
			}
			perPage = n
		case "page":
			n, err := strconv.ParseInt(vals[0], 10, 64)
			if err != nil || n < 1 || n > ownerReposMaxCachedPage {
				return "", "", 0, 0, false
			}
			page = n
		default:
			return "", "", 0, 0, false
		}
	}
	return sort, direction, perPage, page, true
}

// absorbOwnerRepos parses an owner-repos response into the truth rows it
// states and the rendered answer. The rebuild reuses the bare-repo route's
// trimmed object, so a repo reads the same whether a caller asked for it alone
// or in a listing. The absent verdict, meaning the name is not an owner of
// this kind, is stable and is stored; a partial entry is not, and the page
// then relays rather than rendering a hole.
func absorbOwnerRepos(status int, body []byte) ([]dbgen.Repo, ghdata.CachedOwnerRepos, bool) {
	switch status {
	case http.StatusOK:
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			return nil, ghdata.CachedOwnerRepos{}, false
		}
		var raw []json.RawMessage
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return nil, ghdata.CachedOwnerRepos{}, false
		}
		rows := make([]dbgen.Repo, 0, len(raw))
		items := make([]repoMetaJSON, 0, len(raw))
		for _, entry := range raw {
			row, ok := webhook.ParseRepositoryObject(entry)
			if !ok || !repoRowComplete(row) {
				return nil, ghdata.CachedOwnerRepos{}, false
			}
			rows = append(rows, row)
			items = append(items, renderRepoMeta(row))
		}
		doc, err := marshalTrimmed(items)
		if err != nil {
			return nil, ghdata.CachedOwnerRepos{}, false
		}
		return rows, ghdata.CachedOwnerRepos{Status: http.StatusOK, Doc: string(doc)}, true
	case http.StatusNotFound:
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Message == "" {
			e.Message = "Not Found"
		}
		doc, err := marshalTrimmed(notFoundJSON{Message: e.Message, Status: "404"})
		if err != nil {
			return nil, ghdata.CachedOwnerRepos{}, false
		}
		return nil, ghdata.CachedOwnerRepos{Status: http.StatusNotFound, Doc: string(doc)}, true
	default:
		return nil, ghdata.CachedOwnerRepos{}, false
	}
}
