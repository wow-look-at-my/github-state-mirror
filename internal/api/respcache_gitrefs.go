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

// The cached git-ref lookup (tier 2 of the cache contract):
//
//	GET /repos/{owner}/{repo}/git/ref/{ref...}
//
// "Where does this branch point right now?" -- the single hottest UNROUTED
// path in the request log before this route existed, run per branch per sweep
// by the fleet's reconcile passes. The answer is one sha, and every event that
// can change it (create, delete, tip-move) arrives as a push naming the ref,
// which is the cleanest invalidation signal any route here has.
//
// The wildcard is greedy: a ref path is at least two segments ("heads/main")
// and branch names carry slashes ("heads/claude/some-branch"). It is stored
// VERBATIM and never resolved -- "heads/main" and "refs/heads/main" are
// distinct requests, so each is its own row and the push flush covers every
// spelling GitHub accepts (refSpellings, internal/sync/webhook_invalidate.go).
//
// The 404 is absorbed as a VERDICT, on the compare route's precedent: sweeps
// re-poll refs that were deleted (a merged PR's head) forever, each read a
// fresh upstream 404. It stays honest because ref CREATION arrives as a push
// for that exact ref, which drops the verdict row.

const (
	// gitRefCacheTTL bounds how long a MISSED push delivery could leave a
	// stale tip (or a stale absent-verdict) being served. Pushes flush sooner;
	// this is the backstop.
	gitRefCacheTTL = 24 * time.Hour
)

// cachedGitRef serves one ref's tip from a stored snapshot, fetching and
// absorbing on a miss.
func (h *handlers) cachedGitRef(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	ref := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	if len(r.URL.Query()) > 0 {
		// No query parameter changes this answer, so any is unmodeled by
		// definition rather than by omission.
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
	if c, ok, err := h.store.GetCachedGitRef(r.Context(), owner, repo, ref, now); err != nil {
		slog.Warn("git ref cache read failed", "owner", owner, "repo", repo, "ref", ref, "error", err)
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

	status := http.StatusOK
	doc, absorbed := absorbGitRef(resp.StatusCode, body)
	if !absorbed && !overflow && resp.StatusCode == http.StatusNotFound {
		// The absent-ref VERDICT. Bounded the same way the compare route's is:
		// a push that CREATES the ref flushes this exact row, and the TTL
		// backstops a missed delivery.
		if doc404, mErr := marshalTrimmed(notFoundJSON{Message: upstreamErrorMessage(body), Status: "404"}); mErr == nil {
			doc, absorbed, status = string(doc404), true, http.StatusNotFound
		}
	}
	if overflow || !absorbed {
		// 5xx, and the 200 ARRAY form (see gitRefCacheable): relayed
		// verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedGitRef(r.Context(), ghdata.CachedGitRef{
		Owner: owner, Repo: repo, Ref: ref, Status: status, Doc: doc,
	}, now, gitRefCacheTTL); err != nil {
		slog.Warn("git ref cache write failed", "owner", owner, "repo", repo, "ref", ref, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, status, []byte(doc), false)
}

// gitRefCacheable reports whether a requested ref path is one this route
// models. GitHub answers /git/ref/{ref} with a single ref object ONLY for a
// fully-qualified two-part ref ("heads/<name>", "tags/<name>"); a bare or
// partial ref ("heads") makes it behave like matching-refs and answer an
// ARRAY, a different shape this route does not hold. Requiring the qualifier
// keeps one row = one ref object.
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

// gitRefJSON is the trimmed ref answer: the canonical ref name GitHub
// reported, its node_id (a stable identifier, not a link), and the object it
// points at trimmed to sha + type. url, object.url, and every other link
// field are dropped.
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
