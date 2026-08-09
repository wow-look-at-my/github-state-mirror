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

// The cached single-label read (tier 2 of the cache contract):
//
//	GET /repos/{owner}/{repo}/labels/{name}
//
// A label DEFINITION -- "does auto-pr-merge exist here, and what colour is
// it" -- asked per repo per sweep by pr-minder's label reconciliation (1313
// forwards in one process, all of them 200s).
//
// The invalidation signal is exact and already subscribed: `label` deliveries
// carry created/edited/deleted, and the dispatcher already applies them to the
// pr_labels truth rows. The grain here is the REPO rather than the name,
// because a rename moves two names in one delivery and because one label
// answers under every spelling a caller might request. Label events are rare
// and a repo holds a handful of labels, so repo-wide costs nothing.
//
// Only the 200 is stored. The absent answer is deliberately NOT a cached
// verdict: an ensure-then-create caller would read its own stale 404 in the
// seconds before the `label` delivery lands and try to create the label
// twice, and with essentially every observed answer a 200 the verdict would
// buy nothing for that risk.

// labelCacheTTL backstops a missed `label` delivery. Label definitions are
// otherwise static, so this is the long TTL.
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
		// 404 (no such label -- see the header note on why it is not a
		// verdict), 5xx, and any shape the model cannot hold: relayed
		// verbatim, never stored.
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

// writeLabel forwards a label WRITE (recorded as a write, like every proxied
// mutation) after dropping the repo's cached labels. `label` deliveries flush
// these rows too, but they arrive seconds later -- long enough for a caller
// that edits a label through the mirror to read back its own stale answer.
//
// The flush runs BEFORE forwarding, on the Code Quality route's reasoning: a
// failed write that dropped the rows costs one miss, while a successful write
// whose flush was skipped serves a wrong answer for the full TTL.
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
