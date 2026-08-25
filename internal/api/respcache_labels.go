package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The cached single-label read (GET /repos/{owner}/{repo}/labels/{name}); see docs/cache/rest-routes.md.

// labelCacheTTL: see docs/cache/rest-routes.md.
const labelCacheTTL = 24 * time.Hour

// cachedLabel serves one label definition from a stored snapshot, fetching and
// absorbing on a miss.
func (h *handlers) cachedLabel(w http.ResponseWriter, r *http.Request) {
	owner, repo := chi.URLParam(r, "owner"), chi.URLParam(r, "repo")
	name := chi.URLParam(r, "name")

	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	if len(r.URL.Query()) > 0 {
		// The endpoint takes no parameters, so any is unmodeled by definition.
		h.passthrough(w, r, PassQuery)
		return
	}
	if name == "" {
		h.passthrough(w, r, PassPath)
		return
	}

	resourceKey := ghdata.NormalizeRepoKey(owner) + "/" + ghdata.NormalizeRepoKey(repo) + "/labels/" + name
	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindLabel, resourceKey); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if doc, ok, err := h.store.GetCachedLabel(r.Context(), owner, repo, name, now); err != nil {
		slog.Warn("label cache read failed", "owner", owner, "repo", repo, "label", name, "error", err)
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

	doc, absorbed := absorbLabel(resp.StatusCode, body)
	if overflow || !absorbed {
		// 404, 5xx, and any unmodeled shape: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedLabel(r.Context(), owner, repo, name, doc, now, labelCacheTTL); err != nil {
		slog.Warn("label cache write failed", "owner", owner, "repo", repo, "label", name, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// writeLabel flushes the repo's cached labels, then forwards; see docs/cache/rest-routes.md.
func (h *handlers) writeLabel(w http.ResponseWriter, r *http.Request) {
	owner, repo := chi.URLParam(r, "owner"), chi.URLParam(r, "repo")
	if err := h.store.InvalidateLabelCache(r.Context(), owner, repo); err != nil {
		slog.Warn("label flush on write failed", "owner", owner, "repo", repo, "error", err)
	}
	h.ghProxy.ServeHTTP(w, r)
}

// labelJSON is the trimmed rebuild: GitHub's `label` schema minus its one url
// field. `description` is nullable-but-always-keyed and `default` is the flag
// that separates a repo's stock labels from its own.
type labelJSON struct {
	ID          int64   `json:"id"`
	NodeID      string  `json:"node_id"`
	Name        string  `json:"name"`
	Color       string  `json:"color"`
	Default     bool    `json:"default"`
	Description *string `json:"description"`
}

// absorbLabel parses a 200 into the trimmed document, rendered once here so
// hit and miss serve identical bytes. `name` is the field the model requires:
// an answer without it is not the label object this route holds.
func absorbLabel(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false
	}
	var raw struct {
		ID          int64   `json:"id"`
		NodeID      string  `json:"node_id"`
		Name        string  `json:"name"`
		Color       string  `json:"color"`
		Default     bool    `json:"default"`
		Description *string `json:"description"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.Name == "" {
		return "", false
	}
	rendered, err := marshalTrimmed(labelJSON(raw))
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
