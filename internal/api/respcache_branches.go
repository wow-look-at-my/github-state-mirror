package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Cached branches-list route (tier of the cache contract, like respcache.go): GET /repos/{owner}/{repo}/branches
// see docs/cache/rest-routes.md

const (
	// branchesDefaultPerPage is GitHub's default page size when per_page is absent.
	branchesDefaultPerPage = 30

	// branchesMaxCachedPage caps which pages are modeled; deeper pagination passes through.
	branchesMaxCachedPage = 10
)

// parseBranchesListShape reports the paging shape of a /branches query and
// whether the cache models it. Unknown params (protected, ...), repeated
// params, an out-of-range per_page, or a page beyond the modeled cap make it
// non-cacheable.
func parseBranchesListShape(q url.Values) (perPage, page int, ok bool) {
	perPage, page = branchesDefaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return 0, 0, false
		}
		v := vals[0]
		switch key {
		case "per_page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return 0, 0, false
			}
			perPage = n
		case "page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > branchesMaxCachedPage {
				return 0, 0, false
			}
			page = n
		default:
			return 0, 0, false
		}
	}
	return perPage, page, true
}

// cachedBranchesList serves page of a repo's branch list from a stored
// whole-doc snapshot, fetching and absorbing on a miss. Unknown query shapes
// and non-default Accepts pass through.
func (h *handlers) cachedBranchesList(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")

	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	perPage, page, ok := parseBranchesListShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindBranches, ghdata.NormalizeRepoKey(owner)+"/"+ghdata.NormalizeRepoKey(repo)+"/branches"); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if doc, ok, err := h.store.GetCachedBranchesList(r.Context(), owner, repo, int64(perPage), int64(page), now); err != nil {
		slog.Warn("branches list cache read failed", "owner", owner, "repo", repo, "error", err)
	} else if ok {
		h.reqlog.observe(r, DispHit)
		writeRebuilt(w, http.StatusOK, []byte(doc), true)
		return
	}

	// Miss: fetch from GitHub with the caller's own credentials.
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	doc, absorbed := absorbBranchesList(resp.StatusCode, body)
	if overflow || !absorbed {
		// Includes and 5xx: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedBranchesList(r.Context(), owner, repo, int64(perPage), int64(page), doc, now, ghdata.BranchesCacheTTL); err != nil {
		slog.Warn("branches list cache write failed", "owner", owner, "repo", repo, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// branchListItemJSON is trimmed entry: name + commit.sha + the always-present protected bool.
type branchListItemJSON struct {
	Name      string     `json:"name"`
	Commit    gitSHAJSON `json:"commit"`
	Protected bool       `json:"protected"`
}

// absorbBranchesList parses a /branches array into the trimmed document,
// rendered here (hits serve the stored bytes, so hit and miss are
// byte-identical). Reports false -- serve verbatim, store nothing -- for any
// other status or any item the model cannot hold (name and a full-hex tip
// sha are required; protected defaults false). An empty array (a page past
// the end) is a valid, cacheable answer.
func absorbBranchesList(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return "", false
	}
	var raw []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
		Protected bool `json:"protected"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return "", false
	}
	items := make([]branchListItemJSON, 0, len(raw))
	for _, b := range raw {
		sha := strings.ToLower(b.Commit.SHA)
		if b.Name == "" || !isFullHexSHA(sha) {
			return "", false
		}
		items = append(items, branchListItemJSON{
			Name: b.Name, Commit: gitSHAJSON{SHA: sha}, Protected: b.Protected,
		})
	}
	rendered, err := marshalTrimmed(items)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
