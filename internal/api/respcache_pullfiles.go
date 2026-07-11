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

// This file implements the cached PR-files route (tier 2 of the cache
// contract, like respcache.go):
//
//	GET /repos/{owner}/{repo}/pulls/{number}/files
//
// pr-minder's getPullDiff falls back to paging this endpoint when GitHub 406s
// a too-large unified diff, and the App re-reads one PR's files around every
// describe hand-off -- the single largest passthrough slice before this route
// existed. The whole trimmed files page is rendered ONCE at absorb time and
// stored verbatim per exact request shape (owner, repo, number, per_page,
// page), like the compare doc, so hit and miss serve identical bytes. The
// per-file `previous_filename` and `patch` presence is load-bearing:
// consumers test `typeof f.patch === 'string'`, and GitHub legitimately omits
// patch on binary/oversized files -- pointer fields preserve present-vs-absent
// exactly. The per-file blob sha and every URL field (blob_url, raw_url,
// contents_url, _links) are dropped.
//
// `patch` is unbounded, so a rendered document larger than
// pullFilesDocMaxBytes passes through unstored (mirroring the contents
// route's 1 MiB rule); the 8 MiB fetchUpstream cap still bounds the raw body.
// A PR's files move whenever its head or base moves: pull_request events
// flush that one PR's pages (covering fork-head pushes whose push events we
// never see, base retargets, reopens), push/repository events flush the whole
// repo (the belt for missed pull_request deliveries), and the 24h TTL
// backstops everything else.

const (
	// pullFilesCacheTTL bounds how long a MISSED pull_request delivery could
	// leave a stale files page being served. Webhooks flush sooner; this is
	// the backstop.
	pullFilesCacheTTL = 24 * time.Hour

	// pullFilesDefaultPerPage is GitHub's default page size for the PR files
	// list when the request does not send per_page.
	pullFilesDefaultPerPage = 30

	// pullFilesMaxCachedPage caps which pages are modeled. GitHub's files API
	// stops at 3000 files, which is 30 pages at the consumers' per_page=100
	// (pr-minder's assembleDiffFromFiles pages exactly that shape until a
	// short page -- consumer survey 2026-07-11); the margin covers the
	// trailing empty page that ends its loop and smaller per_page shapes.
	// Pages past the cap still pass through.
	pullFilesMaxCachedPage = 40

	// pullFilesDocMaxBytes caps the rendered trimmed document: `patch` is
	// unbounded, and a monster page is not worth a cache row. An over-cap
	// page is a passthrough, not an error.
	pullFilesDocMaxBytes = 1 << 20 // 1 MiB
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

// cachedPullFiles serves one page of a PR's files list from a stored
// whole-doc snapshot, fetching and absorbing on a miss. Non-numeric path
// segments, unknown query shapes, and non-default Accepts pass through.
func (h *handlers) cachedPullFiles(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	numStr := chi.URLParam(r, "number")

	number, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil || number <= 0 || !acceptsDefaultJSON(r) {
		h.ghProxy.ServeHTTP(w, r)
		return
	}
	perPage, page, ok := parsePullFilesShape(r.URL.Query())
	if !ok {
		h.ghProxy.ServeHTTP(w, r)
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

	doc, absorbed := absorbPullFiles(resp.StatusCode, body)
	if overflow || !absorbed {
		// Includes 404 (the PR can be created later), 5xx, and over-cap
		// pages: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedPullFiles(r.Context(), owner, repo, number, int64(perPage), int64(page), doc, now, pullFilesCacheTTL); err != nil {
		slog.Warn("pull files cache write failed", "owner", owner, "repo", repo, "number", number, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// pullFileJSON is one trimmed entry of a PR's files page. previous_filename
// and patch are POINTERS because their presence is load-bearing: GitHub omits
// patch on binary/oversized files (consumers test `typeof f.patch ===
// 'string'`) and previous_filename appears only on renames -- the rebuild
// preserves present-vs-absent exactly. The per-file blob sha and every URL
// field stay dropped.
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

// absorbPullFiles parses a /pulls/{number}/files 200 array into the trimmed
// document, rendered once here (hits serve the stored bytes, so hit and miss
// are byte-identical). Reports false -- serve verbatim, store nothing -- for
// any other status, any item the model cannot hold, or a rendered document
// past the size cap. An empty array (a page past the end) is a valid,
// cacheable answer.
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
	// The unbounded-patch cap (mirrors the contents route's 1 MiB rule): a
	// monster page is not worth a cache row -- passthrough, not an error.
	if len(rendered) > pullFilesDocMaxBytes {
		return "", false
	}
	return string(rendered), true
}
