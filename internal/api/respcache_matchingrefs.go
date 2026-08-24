package api

import (
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

// This file implements the cached ref-prefix-search route (tier 2 of the
// cache contract):
//
//	GET /repos/{owner}/{repo}/git/matching-refs/heads/{prefix}
//
// pr-minder's queue-branch scan is the modeled caller (survey 2026-08-23).
// Only the heads/ prefix form is modeled -- GitHub's endpoint takes any
// refs/ subtree, but every observed call names heads/, and modeling tags/
// or a bare refs/ prefix would be guessing at a shape nothing here sends.
// Per-page whole-doc snapshots, the branches_list_cache precedent: a branch
// create/delete/tip-move all arrive as a push naming a ref under
// refs/heads/, and this route has no narrower per-ref target than the
// branches listing does, so push/repository webhooks flush the whole repo's
// rows rather than attempting a per-prefix apply.

const (
	matchingRefsDefaultPerPage = 30
	// matchingRefsMaxCachedPage caps the modeled pages; deeper pagination
	// passes through, the hooks-route precedent.
	matchingRefsMaxCachedPage = 10
)

// cachedMatchingRefs serves one page of a ref-prefix search.
func (h *handlers) cachedMatchingRefs(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	prefix := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	if prefix == "" || !acceptsDefaultJSON(r) {
		h.passthrough(w, r, shapeReason(r, prefix != ""))
		return
	}
	perPage, page, ok := parseMatchingRefsShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}

	resourceKey := owner + "/" + repo + "/git/matching-refs/heads/" + prefix
	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindMatchingRefs, resourceKey); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if doc, ok, err := h.store.GetCachedMatchingRefs(r.Context(), owner, repo, prefix, perPage, page, now); err != nil {
		slog.Warn("matching refs cache read failed", "owner", owner, "repo", repo, "prefix", prefix, "error", err)
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

	doc, absorbed := absorbMatchingRefs(resp.StatusCode, body)
	if overflow || !absorbed {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedMatchingRefs(r.Context(), owner, repo, prefix, perPage, page, doc, now, ghdata.MatchingRefsCacheTTL); err != nil {
		slog.Warn("matching refs cache write failed", "owner", owner, "repo", repo, "prefix", prefix, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// parseMatchingRefsShape reports the paging shape, the hooks-route pattern.
func parseMatchingRefsShape(q url.Values) (perPage, page int64, ok bool) {
	perPage, page = matchingRefsDefaultPerPage, 1
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
			if n < 1 || n > matchingRefsMaxCachedPage {
				return 0, 0, false
			}
			page = n
		default:
			return 0, 0, false
		}
	}
	return perPage, page, true
}

// matchingRefJSON is one trimmed matching-ref entry: url/node_id dropped,
// object trimmed to its sha (the one field pr-minder's queue scan reads).
type matchingRefJSON struct {
	Ref    string             `json:"ref"`
	Object matchingRefSHAJSON `json:"object"`
}

type matchingRefSHAJSON struct {
	SHA string `json:"sha"`
}

// absorbMatchingRefs parses a 200 matching-refs listing into the trimmed
// document. The body must be an ARRAY; an empty array (no ref matches the
// prefix) is a valid cacheable answer.
func absorbMatchingRefs(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	var raw []struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", false
	}
	out := make([]matchingRefJSON, 0, len(raw))
	for _, item := range raw {
		if item.Ref == "" {
			return "", false
		}
		out = append(out, matchingRefJSON{Ref: item.Ref, Object: matchingRefSHAJSON{SHA: item.Object.SHA}})
	}
	rendered, err := marshalTrimmed(out)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
