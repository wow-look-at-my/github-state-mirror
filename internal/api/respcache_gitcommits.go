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

// This file implements the cached git-commit route (tier 2 of the cache
// contract, like respcache.go, which it was split out of):
//
//	GET /repos/{owner}/{repo}/git/commits/{sha}
//
// isFullHexSHA (shared by the commits-list, commit-CI, and workflow-runs
// routes) lives here with its primary user.

// gitCommitMissTTL bounds a cached git-commit 404 verdict. Round 1
// deliberately did NOT cache these ("a missing sha can be pushed later");
// round 2 bounds that concern with this expiry PLUS the clear-on-upsert
// invariant (every real commit absorb -- fetch, push payload, commits-list,
// compare -- clears its sha's marker via ghdata.upsertGitCommit), because
// the dominant traffic is pr-minder's mergeWouldBeEmpty re-reading GC'd
// test-merge shas on every fleet sweep: a sha GitHub has garbage-collected
// 404s FOREVER, and each of those reads used to be a fresh upstream 404.
// Consumers fail open on a 404 (mergeWouldBeEmpty treats it as "cannot
// verify, run the update"), so the rare wrong-marker window -- a sha pushed
// while its marker lives but before any absorb path sees the commit -- is
// safe as well as bounded.
const gitCommitMissTTL = 24 * time.Hour

// cachedGitCommit serves a git commit from absorbed state. Commits are
// immutable, so cached POSITIVE rows never expire and no webhook invalidates
// them — only LRU pruning bounds the table. Rows are also absorbed from push
// webhook payloads (internal/sync/webhook.go), so the common post-push read
// can hit without any GitHub fetch ever having happened. A 404 answer is
// cached as an EXPIRING miss marker (see gitCommitMissTTL) that any real
// absorb of the sha clears.
func (h *handlers) cachedGitCommit(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	sha := strings.ToLower(chi.URLParam(r, "sha"))

	// Only full hex object ids are cache keys (a short sha is ambiguous over
	// time); the endpoint takes no query params. Anything else — passthrough.
	if !acceptsDefaultJSON(r) || r.URL.RawQuery != "" || !isFullHexSHA(sha) {
		h.ghProxy.ServeHTTP(w, r)
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindGitCommit, owner+"/"+repo+"@"+sha); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if c, ok, err := h.store.GetCachedGitCommit(r.Context(), owner, repo, sha, now); err == nil && ok {
		h.serveGitCommit(w, r, c, true)
		return
	} else if err != nil {
		slog.Warn("git commit cache read failed", "owner", owner, "repo", repo, "sha", sha, "error", err)
	}

	// A live 404 miss marker answers before any upstream fetch -- the GC'd
	// test-merge sha pr-minder re-reads forever (see gitCommitMissTTL).
	if doc, ok, err := h.store.GetCachedGitCommitMiss(r.Context(), owner, repo, sha, now); err != nil {
		slog.Warn("git commit miss cache read failed", "owner", owner, "repo", repo, "sha", sha, "error", err)
	} else if ok {
		h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispHit, http.StatusNotFound)
		writeRebuilt(w, http.StatusNotFound, []byte(doc), true)
		return
	}

	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	c, absorbed := absorbGitCommit(owner, repo, sha, resp.StatusCode, body)
	if overflow || !absorbed {
		// A 404 is absorbed as an expiring miss marker and relayed REBUILT
		// (the contents route's exact 404 treatment: DispMiss with the
		// upstream status, the notFoundJSON shape). Everything else -- 5xx,
		// unexpected shapes -- relays unstored.
		if !overflow && resp.StatusCode == http.StatusNotFound {
			if doc, mErr := marshalTrimmed(notFoundJSON{Message: upstreamErrorMessage(body), Status: "404"}); mErr == nil {
				if err := h.store.PutCachedGitCommitMiss(r.Context(), owner, repo, sha, string(doc), now, gitCommitMissTTL); err != nil {
					slog.Warn("git commit miss cache write failed", "owner", owner, "repo", repo, "sha", sha, "error", err)
				}
				h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispMiss, resp.StatusCode)
				writeRebuilt(w, http.StatusNotFound, doc, false)
				return
			}
		}
		h.replayUnstored(w, r, resp, body)
		return
	}
	// A positive absorb also clears any 404 marker for the sha (the
	// clear-on-upsert invariant inside ghdata.upsertGitCommit).
	if err := h.store.PutCachedGitCommit(r.Context(), c, now); err != nil {
		slog.Warn("git commit cache write failed", "owner", owner, "repo", repo, "sha", sha, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispMiss, resp.StatusCode)
	h.serveGitCommit(w, r, c, false)
}

func (h *handlers) serveGitCommit(w http.ResponseWriter, r *http.Request, c ghdata.CachedGitCommit, hit bool) {
	body, err := marshalTrimmed(renderGitCommit(c))
	if err != nil {
		slog.Warn("git commit cache render failed", "sha", c.SHA, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hit {
		h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, DispHit)
	}
	writeRebuilt(w, http.StatusOK, body, hit)
}

// gitPersonJSON is a commit author/committer identity.
type gitPersonJSON struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

// gitSHAJSON is a bare object reference ({"sha": ...}).
type gitSHAJSON struct {
	SHA string `json:"sha"`
}

// gitCommitJSON is the trimmed rebuild of a git commit: one consistent shape
// regardless of source (upstream fetch or push webhook). Fields GitHub has
// that we drop — node_id, verification, url/html_url and the tree/parent
// urls — stay dropped.
type gitCommitJSON struct {
	SHA       string        `json:"sha"`
	Author    gitPersonJSON `json:"author"`
	Committer gitPersonJSON `json:"committer"`
	Message   string        `json:"message"`
	Tree      gitSHAJSON    `json:"tree"`
	Parents   []gitSHAJSON  `json:"parents"`
}

func renderGitCommit(c ghdata.CachedGitCommit) gitCommitJSON {
	parents := make([]gitSHAJSON, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, gitSHAJSON{SHA: p})
	}
	return gitCommitJSON{
		SHA:       c.SHA,
		Author:    gitPersonJSON{Name: c.AuthorName, Email: c.AuthorEmail, Date: c.AuthorDate},
		Committer: gitPersonJSON{Name: c.CommitterName, Email: c.CommitterEmail, Date: c.CommitterDate},
		Message:   c.Message,
		Tree:      gitSHAJSON{SHA: c.TreeSHA},
		Parents:   parents,
	}
}

// absorbGitCommit parses an upstream git-commit response into cacheable state.
// Only a well-formed 200 is absorbed.
func absorbGitCommit(owner, repo, sha string, status int, body []byte) (ghdata.CachedGitCommit, bool) {
	if status != http.StatusOK {
		return ghdata.CachedGitCommit{}, false
	}
	var g struct {
		SHA     string `json:"sha"`
		Message string `json:"message"`
		Author  struct {
			Name  string `json:"name"`
			Email string `json:"email"`
			Date  string `json:"date"`
		} `json:"author"`
		Committer struct {
			Name  string `json:"name"`
			Email string `json:"email"`
			Date  string `json:"date"`
		} `json:"committer"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := json.Unmarshal(body, &g); err != nil || g.SHA == "" || g.Tree.SHA == "" {
		return ghdata.CachedGitCommit{}, false
	}
	parents := make([]string, 0, len(g.Parents))
	for _, p := range g.Parents {
		parents = append(parents, p.SHA)
	}
	return ghdata.CachedGitCommit{
		Owner: owner, Repo: repo, SHA: sha, Message: g.Message,
		AuthorName: g.Author.Name, AuthorEmail: g.Author.Email, AuthorDate: g.Author.Date,
		CommitterName: g.Committer.Name, CommitterEmail: g.Committer.Email, CommitterDate: g.Committer.Date,
		TreeSHA: g.Tree.SHA, Parents: parents,
	}, true
}

// isFullHexSHA reports whether s is a full-length (40 or 64) lowercase hex
// object id.
func isFullHexSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
