package api

import (
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The cached PR commits list (tier 2): GET /repos/{owner}/{repo}/pulls/{number}/commits, reusing the repo commits-list storage whole.
// see docs/cache/rest-routes.md

const (
	pullCommitsDefaultPerPage = 30
	// pullCommitsMaxCachedPage caps modeled pages (GitHub's PR-commits list stops at 250); deeper pagination passes through.
	pullCommitsMaxCachedPage = 10
)

func pullCommitsRefKey(number int64) string {
	return "pull/" + strconv.FormatInt(number, 10) + "/commits"
}

// cachedPullCommits serves one page of a PR's commits, fetching and absorbing
// on a miss.
func (h *handlers) cachedPullCommits(w http.ResponseWriter, r *http.Request) {
	owner, repo := chi.URLParam(r, "owner"), chi.URLParam(r, "repo")
	number, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
	if err != nil || number <= 0 {
		// /pulls/comments/... and friends: not this route's shape.
		h.passthrough(w, r, PassPath)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	perPage, page, ok := parsePullCommitsShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}

	// The PR is the resource being read, so this shares the single-PR route's deny kind and resource key.
	resourceKey := ghdata.NormalizeRepoKey(owner) + "/" + ghdata.NormalizeRepoKey(repo) +
		"/pull/" + strconv.FormatInt(number, 10)
	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindPull, resourceKey); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	refKey := pullCommitsRefKey(number)
	now := time.Now()
	if commits, ok, err := h.store.GetCachedCommitsList(r.Context(), owner, repo, refKey, perPage, page, now); err != nil {
		slog.Warn("pull commits cache read failed", "owner", owner, "repo", repo, "pr", number, "error", err)
	} else if ok {
		h.serveCommitsList(w, r, commits, true)
		return
	}

	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	commits, absorbed := absorbCommitsList(owner, repo, resp.StatusCode, body)
	if overflow || !absorbed {
		// Includes 404 (unknown PR, can be opened later) and 5xx: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedCommitsList(r.Context(), owner, repo, refKey, perPage, page, commits, now, commitsListCacheTTL); err != nil {
		slog.Warn("pull commits absorb failed", "owner", owner, "repo", repo, "pr", number, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	h.serveCommitsList(w, r, commits, false)
}

// parsePullCommitsShape reports the paging shape and whether the cache models
// it. Only per_page/page are modeled; this endpoint takes no filters, so
// anything else is a shape the route has no business guessing at.
func parsePullCommitsShape(q url.Values) (perPage, page int, ok bool) {
	perPage, page = pullCommitsDefaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return 0, 0, false
		}
		switch key {
		case "per_page":
			n, err := strconv.Atoi(vals[0])
			if err != nil || n < 1 || n > 100 {
				return 0, 0, false
			}
			perPage = n
		case "page":
			n, err := strconv.Atoi(vals[0])
			if err != nil || n < 1 || n > pullCommitsMaxCachedPage {
				return 0, 0, false
			}
			page = n
		default:
			return 0, 0, false
		}
	}
	return perPage, page, true
}
