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

// Cached git-ref lookup (tier 2 of the cache contract): GET /repos/{owner}/{repo}/git/ref/{ref...}
// see docs/cache/rest-routes.md and docs/cache/stale-tip-repair.md

// gitRefCacheTTL is only the backstop for a missed push delivery; a delivered push applies its own tip.
// see docs/cache/stale-tip-repair.md
const gitRefCacheTTL = ghdata.GitRefCacheTTL

// cachedGitRef serves one ref's tip from a stored snapshot, fetching and
// absorbing on a miss -- or when the stored snapshot contradicts something
// else this mirror holds (see the contradiction check below).
func (h *handlers) cachedGitRef(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	ref := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	if len(r.URL.Query()) > 0 {
		// No query parameter changes this answer, so any is unmodeled.
		h.passthrough(w, r, PassQuery)
		return
	}
	if !gitRefCacheable(ref) {
		h.passthrough(w, r, PassPath)
		return
	}

	resourceKey := ghdata.NormalizeRepoKey(owner) + "/" + ghdata.NormalizeRepoKey(repo) + "/git/ref/" + ref
	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindGitRef, resourceKey); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	// A contradiction is a reason to ask GitHub, not a verdict; it rides along so the refetched row records what settled it.
	contradiction := ""
	if c, ok, why, err := h.store.GetCachedGitRefChecked(r.Context(), owner, repo, ref, now); err != nil {
		slog.Warn("git ref cache read failed", "owner", owner, "repo", repo, "ref", ref, "error", err)
	} else if ok && why == "" {
		h.reqlog.observe(r, DispHit)
		writeRebuilt(w, c.Status, []byte(c.Doc), true)
		return
	} else if ok {
		contradiction = why
		slog.Info("git ref cache contradicted by an absorbed PR base -- refetching rather than serving it",
			"owner", owner, "repo", repo, "ref", ref, "stored", c.Doc, "pr_base", why)
	}

	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	status := http.StatusOK
	doc, absorbed := absorbGitRef(resp.StatusCode, body)
	if !absorbed && !overflow && resp.StatusCode == http.StatusNotFound {
		// The absent-ref verdict: a ref-creating push flushes this row; the TTL backstops a missed delivery.
		if doc404, mErr := marshalTrimmed(notFoundJSON{Message: upstreamErrorMessage(body), Status: "404"}); mErr == nil {
			doc, absorbed, status = string(doc404), true, http.StatusNotFound
		}
	}
	if overflow || !absorbed {
		// 5xx and the 200 ARRAY form (see gitRefCacheable): relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedGitRef(r.Context(), ghdata.CachedGitRef{
		Owner: owner, Repo: repo, Ref: ref, Status: status, Doc: doc,
		// The settled contradiction, so the same lagging base.sha does not buy a refetch every time.
		ReconciledAgainst: contradiction,
	}, now, gitRefCacheTTL); err != nil {
		slog.Warn("git ref cache write failed", "owner", owner, "repo", repo, "ref", ref, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, status, []byte(doc), false)
}

// gitRefCacheable requires a fully-qualified two-part ref; a bare ref answers an ARRAY, a shape this route does not hold.
func gitRefCacheable(ref string) bool {
	if ref == "" || strings.ContainsAny(ref, "?#") {
		return false
	}
	rest, ok := strings.CutPrefix(ref, "refs/")
	if !ok {
		rest = ref
	}
	kind, name, found := strings.Cut(rest, "/")
	if !found || name == "" {
		return false
	}
	return kind == "heads" || kind == "tags"
}

// gitRefJSON is the trimmed ref answer: ref, node_id, and the object trimmed to sha + type.
type gitRefJSON struct {
	Ref    string        `json:"ref"`
	NodeID string        `json:"node_id"`
	Object gitRefObjJSON `json:"object"`
}

type gitRefObjJSON struct {
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

// absorbGitRef parses a /git/ref/{ref} 200 into the trimmed document,
// rendered once here so hit and miss serve identical bytes. Reports false --
// serve verbatim, store nothing -- for any other status, for the ARRAY form
// (a partial ref, which the route guard already refuses), and for a body
// missing the ref name or a full-hex object sha.
func absorbGitRef(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false
	}
	var raw struct {
		Ref    string `json:"ref"`
		NodeID string `json:"node_id"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return "", false
	}
	sha := strings.ToLower(raw.Object.SHA)
	if raw.Ref == "" || raw.Object.Type == "" || !isFullHexSHA(sha) {
		return "", false
	}
	rendered, err := marshalTrimmed(gitRefJSON{
		Ref: raw.Ref, NodeID: raw.NodeID,
		Object: gitRefObjJSON{SHA: sha, Type: raw.Object.Type},
	})
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
