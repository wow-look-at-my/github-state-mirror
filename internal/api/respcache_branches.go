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

// This file implements the cached branches-list route (tier 2 of the cache
// contract, like respcache.go):
//
//	GET /repos/{owner}/{repo}/branches
//
// pr-minder's auto_open_pr fork-point detection lists every branch tip
// (listBranchHeads) per repo, and the pr-minder-reconcile hook repeats that
// over a fleet slice every 5 minutes -- the second-largest passthrough slice
// before this route existed. The whole trimmed branches page is rendered
// ONCE at absorb time and stored verbatim per exact request shape (owner,
// repo, per_page, page), like the compare doc, so hit and miss serve
// identical bytes. Consumers read name + commit.sha; protected rides along
// (a cheap, always-present bool). commit.url, the protection object, and
// protection_url are dropped.
//
// A listing moves whenever a branch is created, deleted, or its tip
// advances -- all of which arrive as push events (a delete carries
// deleted=true) -- so push/repository webhooks flush a repo's snapshots
// repo-wide, with the 24h TTL as the missed-delivery backstop. There is no
// doc-size cap: the item shape has no unbounded field, and the 8 MiB
// fetchUpstream cap bounds the raw body. The single-branch read
// /branches/{branch} is a different shape and stays passthrough.

const (
	// branchesCacheTTL bounds how long a MISSED push delivery could leave a
	// stale listing being served. Webhooks flush sooner; this is the
	// backstop.
	branchesCacheTTL = 24 * time.Hour

	// branchesDefaultPerPage is GitHub's default page size for the branches
	// list when the request does not send per_page.
	branchesDefaultPerPage = 30

	// branchesMaxCachedPage caps which pages are modeled. Consumers page
	// shallowly; deeper pagination passes through.
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

// cachedBranchesList serves one page of a repo's branch list from a stored
// whole-doc snapshot, fetching and absorbing on a miss. Unknown query shapes
// and non-default Accepts pass through.
func (h *handlers) cachedBranchesList(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")

	if !acceptsDefaultJSON(r) {
		h.ghProxy.ServeHTTP(w, r)
		return
	}
	perPage, page, ok := parseBranchesListShape(r.URL.Query())
	if !ok {
		h.ghProxy.ServeHTTP(w, r)
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
		h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, DispHit)
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
		// Includes 404 and 5xx: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedBranchesList(r.Context(), owner, repo, int64(perPage), int64(page), doc, now, branchesCacheTTL); err != nil {
		slog.Warn("branches list cache write failed", "owner", owner, "repo", repo, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// branchListItemJSON is one trimmed entry of the branches page: exactly what
// the consumers read (name + commit.sha) plus the always-present protected
// bool. commit.url, the protection object, and protection_url stay dropped.
type branchListItemJSON struct {
	Name      string     `json:"name"`
	Commit    gitSHAJSON `json:"commit"`
	Protected bool       `json:"protected"`
}

// absorbBranchesList parses a /branches 200 array into the trimmed document,
// rendered once here (hits serve the stored bytes, so hit and miss are
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
