package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file implements the cached git-tree route (tier of the cache
// contract):
//
//	GET /repos/{owner}/{repo}/git/trees/{sha}[?recursive=]
//
// Trees are immutable and content-addressed by sha, the git_commits_cache
// precedent: no TTL, no webhook invalidation (no delivery ever names a tree
// object directly), LRU pruning only. recursive is the verbatim query value
// and part of the row key, because GitHub answers a DIFFERENT entry set for
// the same sha depending on it -- it is not a rendering option, it changes
// what the resource IS.

// cachedGitTree serves a git tree from absorbed state.
func (h *handlers) cachedGitTree(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	sha := strings.ToLower(chi.URLParam(r, "sha"))

	if !isFullHexSHA(sha) || !acceptsDefaultJSON(r) {
		h.passthrough(w, r, shapeReason(r, isFullHexSHA(sha)))
		return
	}
	recursive, ok := parseGitTreeShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindGitTree, owner+"/"+repo+"@"+sha+"#"+recursive); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if c, ok, err := h.store.GetCachedGitTree(r.Context(), owner, repo, sha, recursive, now); err == nil && ok {
		h.reqlog.observe(r, DispHit)
		writeRebuilt(w, http.StatusOK, []byte(c.Doc), true)
		return
	} else if err != nil {
		slog.Warn("git tree cache read failed", "owner", owner, "repo", repo, "sha", sha, "error", err)
	}

	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	doc, absorbed := absorbGitTree(resp.StatusCode, body)
	if overflow || !absorbed {
		// A (bad/expired sha) or anything unmodeled relays unstored -- GitHub GC'ing a tree is too rare to need a miss-marker cache.
		h.replayUnstored(w, r, resp, body)
		return
	}
	c := ghdata.CachedGitTree{Owner: owner, Repo: repo, SHA: sha, Recursive: recursive, Doc: doc}
	if err := h.store.PutCachedGitTree(r.Context(), c, now); err != nil {
		slog.Warn("git tree cache write failed", "owner", owner, "repo", repo, "sha", sha, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// parseGitTreeShape reports the modeled recursive value: absent, or
// recursive= (the only value GitHub's docs define; any other value is
// GitHub's own to reject, so it stays unmodeled and passes through), and no
// other query parameter.
func parseGitTreeShape(q map[string][]string) (recursive string, ok bool) {
	for key, vals := range q {
		if len(vals) != 1 {
			return "", false
		}
		if key != "recursive" {
			return "", false
		}
		if vals[0] != "1" {
			return "", false
		}
		recursive = "1"
	}
	return recursive, true
}

// gitTreeEntryJSON drops url; Size is omitted unless GitHub sent it (blobs carry it, trees/commits do not).
type gitTreeEntryJSON struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size *int64 `json:"size,omitempty"`
}

// gitTreeJSON is the trimmed rebuild of a git tree.
type gitTreeJSON struct {
	SHA       string             `json:"sha"`
	Tree      []gitTreeEntryJSON `json:"tree"`
	Truncated bool               `json:"truncated"`
}

// absorbGitTree parses and renders an upstream tree response. Only a
// well-formed is absorbed.
func absorbGitTree(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	var g struct {
		SHA  string `json:"sha"`
		Tree []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size *int64 `json:"size"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(body, &g); err != nil || g.SHA == "" {
		return "", false
	}
	out := gitTreeJSON{SHA: g.SHA, Truncated: g.Truncated, Tree: make([]gitTreeEntryJSON, 0, len(g.Tree))}
	for _, e := range g.Tree {
		out.Tree = append(out.Tree, gitTreeEntryJSON{Path: e.Path, Mode: e.Mode, Type: e.Type, SHA: e.SHA, Size: e.Size})
	}
	rendered, err := marshalTrimmed(out)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
