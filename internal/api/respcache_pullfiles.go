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
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Implements the cached PR-files route (tier of the cache contract).
// See docs/cache/rest-routes.md for the shape, size cap, and flush rules.

const (
	// pullFilesCacheTTL backstops a MISSED pull_request delivery; webhooks flush sooner.
	pullFilesCacheTTL = 24 * time.Hour

	// pullFilesDefaultPerPage is GitHub's default page size when per_page is omitted.
	pullFilesDefaultPerPage = 30

	// pullFilesMaxCachedPage caps modeled pages at GitHub's -file ceiling plus margin; deeper pages pass through.
	pullFilesMaxCachedPage = 40

	// pullFilesDocMaxBytes caps the rendered document; an over-cap page passes through, not an error.
	pullFilesDocMaxBytes = 1 << 20 //  MiB
)

// parsePullFilesShape reports the paging shape of a /pulls/{number}/files
// query and whether the cache models it. Unknown params, repeated params, an
// out-of-range per_page, or a page beyond the modeled cap make it
// non-cacheable.
func parsePullFilesShape(q url.Values) (perPage, page int, ok bool) {
	perPage, page = pullFilesDefaultPerPage, 1
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
			if err != nil || n < 1 || n > pullFilesMaxCachedPage {
				return 0, 0, false
			}
			page = n
		default:
			return 0, 0, false
		}
	}
	return perPage, page, true
}

// cachedPullFiles serves page of a PR's files list from a stored
// whole-doc snapshot, fetching and absorbing on a miss. Non-numeric path
// segments, unknown query shapes, and non-default Accepts pass through.
func (h *handlers) cachedPullFiles(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	numStr := chi.URLParam(r, "number")

	number, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil || number <= 0 || !acceptsDefaultJSON(r) {
		h.passthrough(w, r, shapeReason(r, err == nil && number > 0))
		return
	}
	perPage, page, ok := parsePullFilesShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindPullFiles, ghdata.NormalizeRepoKey(owner)+"/"+ghdata.NormalizeRepoKey(repo)+"#"+numStr); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if doc, ok, err := h.store.GetCachedPullFiles(r.Context(), owner, repo, number, int64(perPage), int64(page), now); err != nil {
		slog.Warn("pull files cache read failed", "owner", owner, "repo", repo, "number", number, "error", err)
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

	doc, absorbed := absorbPullFiles(resp.StatusCode, body)
	if overflow || !absorbed {
		// Includes, 5xx, and over-cap pages: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedPullFiles(r.Context(), owner, repo, number, int64(perPage), int64(page), doc, now, pullFilesCacheTTL); err != nil {
		slog.Warn("pull files cache write failed", "owner", owner, "repo", repo, "number", number, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// pullFileJSON keeps previous_filename and patch as pointers so their presence, not just value, survives the trim.
type pullFileJSON struct {
	Filename         string  `json:"filename"`
	Status           string  `json:"status"`
	Additions        int64   `json:"additions"`
	Deletions        int64   `json:"deletions"`
	Changes          int64   `json:"changes"`
	PreviousFilename *string `json:"previous_filename,omitempty"`
	Patch            *string `json:"patch,omitempty"`
}

// pullFileUpstreamJSON is the GitHub-shaped files-list item, parsing only
// what the trim keeps plus the required-field checks.
type pullFileUpstreamJSON struct {
	Filename         string  `json:"filename"`
	Status           string  `json:"status"`
	Additions        int64   `json:"additions"`
	Deletions        int64   `json:"deletions"`
	Changes          int64   `json:"changes"`
	PreviousFilename *string `json:"previous_filename"`
	Patch            *string `json:"patch"`
}

// absorbPullFiles renders the trimmed document, so a hit and a miss serve byte-identical bytes.
func absorbPullFiles(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return "", false
	}
	var raw []pullFileUpstreamJSON
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return "", false
	}
	items := make([]pullFileJSON, 0, len(raw))
	for _, f := range raw {
		if f.Filename == "" || f.Status == "" {
			return "", false
		}
		items = append(items, pullFileJSON{
			Filename: f.Filename, Status: f.Status,
			Additions: f.Additions, Deletions: f.Deletions, Changes: f.Changes,
			PreviousFilename: f.PreviousFilename, Patch: f.Patch,
		})
	}
	rendered, err := marshalTrimmed(items)
	if err != nil {
		return "", false
	}
	// Over-cap is a passthrough, not an error: patch is unbounded.
	if len(rendered) > pullFilesDocMaxBytes {
		return "", false
	}
	return string(rendered), true
}
