package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The cached README read:
//
//	GET /repos/{owner}/{repo}/readme[/{dir}][?ref=]
//
// GitHub answers the content object the contents route holds, so the rebuild
// reuses contentsFileJSON. The absent-README verdict is stored too: it is an
// authoritative answer about the repo, and a fleet polling every repo's README
// spends a large minority of those calls on repos that have none.
// see docs/cache/rest-routes.md

// cachedReadme serves a repo's README from a stored snapshot, fetching and
// absorbing on a miss.
func (h *handlers) cachedReadme(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	dir := strings.Trim(chi.URLParam(r, "*"), "/")

	if !acceptsDefaultJSON(r) {
		// The raw and html representations are a different document; they stay unmodeled and forward.
		h.passthrough(w, r, PassAccept)
		return
	}
	// The endpoint takes the ref parameter and nothing else; anything else is unmodeled.
	q := r.URL.Query()
	ref := q.Get("ref")
	delete(q, "ref")
	if len(q) > 0 {
		h.passthrough(w, r, PassQuery)
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindReadme, readmeResourceKey(owner, repo, dir, ref)); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if c, ok, err := h.store.GetCachedReadme(r.Context(), owner, repo, dir, ref, now); err != nil {
		slog.Warn("readme cache read failed", "owner", owner, "repo", repo, "dir", dir, "error", err)
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

	c, absorbed := absorbReadme(resp.StatusCode, body)
	if overflow || !absorbed {
		// A symlink object, a transient failure, an oversized body: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedReadme(r.Context(), owner, repo, dir, ref, c, now, ghdata.ReadmeCacheTTL); err != nil {
		slog.Warn("readme cache write failed", "owner", owner, "repo", repo, "dir", dir, "error", err)
	}
	// A 2xx is fresh proof of access. The absent verdict is about the README, never the repo, so it proves nothing either way.
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, c.Status, []byte(c.Doc), false)
}

// readmeResourceKey names the resource for the deny cache.
func readmeResourceKey(owner, repo, dir, ref string) string {
	return owner + "/" + repo + "/readme/" + dir + "@" + ref
}

// absorbReadme parses an upstream README response into the stored answer. path
// and name come from the PAYLOAD: the request names a directory, never a file.
func absorbReadme(status int, body []byte) (ghdata.CachedReadme, bool) {
	switch status {
	case http.StatusOK:
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return ghdata.CachedReadme{}, false
		}
		var f struct {
			Type     string  `json:"type"`
			Encoding string  `json:"encoding"`
			Size     int64   `json:"size"`
			Name     string  `json:"name"`
			Path     string  `json:"path"`
			Content  *string `json:"content"`
			SHA      string  `json:"sha"`
		}
		if err := json.Unmarshal(trimmed, &f); err != nil {
			return ghdata.CachedReadme{}, false
		}
		// The oversized "encoding":"none" form carries no content and is not modeled.
		if f.Type != "file" || f.Encoding != "base64" || f.Content == nil || f.SHA == "" {
			return ghdata.CachedReadme{}, false
		}
		doc, err := marshalTrimmed(contentsFileJSON{
			Type: "file", Encoding: f.Encoding, Size: f.Size,
			Name: f.Name, Path: f.Path, Content: *f.Content, SHA: f.SHA,
		})
		if err != nil {
			return ghdata.CachedReadme{}, false
		}
		return ghdata.CachedReadme{Status: http.StatusOK, Doc: string(doc)}, true
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
			return ghdata.CachedReadme{}, false
		}
		return ghdata.CachedReadme{Status: http.StatusNotFound, Doc: string(doc)}, true
	default:
		return ghdata.CachedReadme{}, false
	}
}
