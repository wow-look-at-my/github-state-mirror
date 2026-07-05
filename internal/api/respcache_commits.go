package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file implements the cached commits LIST route (tier 2 of the cache
// contract, like respcache.go):
//
//	GET /repos/{owner}/{repo}/commits
//
// pr-minder's fork-point base detection pages commit lists per branch
// (?sha=<ref>&per_page=100&page=N, stopping on a short page -- no Link-header
// dependency), dozens of times per sweep, and every one used to pass through.
// Following the absorb-don't-byte-cache doctrine, each listed commit is
// absorbed into the SAME global git_commits_cache rows the single git-commit
// route and push payloads maintain; a commits_list_cache snapshot per exact
// modeled query shape (raw ?sha= value, per_page, page) stores only the
// response's sha ORDER -- the proof that makes rebuilding a LIST from
// immutable commit rows sound. A hit requires the unexpired snapshot AND
// every listed commit row (an LRU-pruned commit degrades to a miss). Unlike
// the pulls list there is no full-page truncation guard: the snapshot IS that
// exact page's answer, and pagination continues under the next page's own
// key. Listings are ref-tip-relative, so push/repository webhooks flush a
// repo's snapshots, with a 24h TTL backstop.
//
// The single-commit endpoint GET /repos/{owner}/{repo}/commits/{sha} (a
// different response shape; pr-minder's commitAgeSeconds) is deliberately NOT
// registered and keeps passing through.

const (
	// commitsListCacheTTL bounds how long a MISSED push delivery could leave a
	// stale snapshot being served. Webhooks flush sooner; this is the backstop.
	commitsListCacheTTL = 24 * time.Hour

	// commitsDefaultPerPage is GitHub's default page size for the commits list
	// when the request does not send per_page.
	commitsDefaultPerPage = 30

	// commitsMaxCachedPage caps which pages are modeled. Consumers page
	// shallowly (pr-minder reads at most 2); deeper pagination passes through.
	commitsMaxCachedPage = 10
)

// commitsListShape is a parsed, cacheable /commits query: the shape
// pr-minder's fork-point detection sends (sha=<ref> + per_page/page) plus the
// bare default. Anything else passes through.
type commitsListShape struct {
	refParam string // raw ?sha= value ('' = default branch)
	perPage  int
	page     int
}

// parseCommitsListShape reports the shape of a /commits query and whether the
// cache models it. Unknown params (path, since, until, author, committer,
// first_parent, ...), repeated params, an empty sha value, an out-of-range
// per_page, or a page beyond the modeled cap make it non-cacheable.
func parseCommitsListShape(q url.Values) (commitsListShape, bool) {
	shape := commitsListShape{perPage: commitsDefaultPerPage, page: 1}
	for key, vals := range q {
		if len(vals) != 1 {
			return shape, false
		}
		v := vals[0]
		switch key {
		case "sha":
			if v == "" {
				return shape, false
			}
			shape.refParam = v
		case "per_page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return shape, false
			}
			shape.perPage = n
		case "page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > commitsMaxCachedPage {
				return shape, false
			}
			shape.page = n
		default:
			return shape, false
		}
	}
	return shape, true
}

// cachedCommitsList serves one page of a repo's commit list from absorbed
// state, fetching and absorbing on a miss.
func (h *handlers) cachedCommitsList(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))

	if !acceptsDefaultJSON(r) {
		h.ghProxy.ServeHTTP(w, r)
		return
	}
	shape, ok := parseCommitsListShape(r.URL.Query())
	if !ok {
		h.ghProxy.ServeHTTP(w, r)
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindRepoCommits, owner+"/"+repo+"/commits"); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if commits, ok, err := h.store.GetCachedCommitsList(r.Context(), owner, repo, shape.refParam, shape.perPage, shape.page, now); err != nil {
		slog.Warn("commits list cache read failed", "owner", owner, "repo", repo, "error", err)
	} else if ok {
		h.serveCommitsList(w, r, commits, true)
		return
	}

	// Miss: fetch from GitHub with the caller's own credentials.
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	commits, absorbed := absorbCommitsList(owner, repo, resp.StatusCode, body)
	if overflow || !absorbed {
		// Includes 404 (unknown ref -- it can be pushed later), 409 (empty
		// repo), and 5xx: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedCommitsList(r.Context(), owner, repo, shape.refParam, shape.perPage, shape.page, commits, now, commitsListCacheTTL); err != nil {
		slog.Warn("commits list absorb failed", "owner", owner, "repo", repo, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispMiss, resp.StatusCode)
	h.serveCommitsList(w, r, commits, false)
}

// serveCommitsList rebuilds and writes the trimmed list. Hit and miss serve
// the same shape in the same (response = snapshot) order.
func (h *handlers) serveCommitsList(w http.ResponseWriter, r *http.Request, commits []ghdata.CachedGitCommit, hit bool) {
	items := make([]commitListItemJSON, 0, len(commits))
	for _, c := range commits {
		items = append(items, renderCommitListItem(c))
	}
	body, err := marshalTrimmed(items)
	if err != nil {
		slog.Warn("commits list render failed", "path", r.URL.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hit {
		h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, DispHit)
	}
	writeRebuilt(w, http.StatusOK, body, hit)
}

// commitDetailJSON is the git-data half of a list item. The REST list nests
// the commit identities under `commit`, unlike the git-commit route's
// top-level shape -- same stored state, two rebuild layouts.
type commitDetailJSON struct {
	Author    gitPersonJSON `json:"author"`
	Committer gitPersonJSON `json:"committer"`
	Message   string        `json:"message"`
	Tree      gitSHAJSON    `json:"tree"`
}

// commitListItemJSON is the trimmed rebuild of one commits-list item: a
// superset of what pr-minder + the pr-minder-reconcile hook read (only the
// top-level sha). GitHub's node_id, comment_count, verification, the
// top-level author/committer USER objects, and every URL field stay dropped.
type commitListItemJSON struct {
	SHA     string           `json:"sha"`
	Commit  commitDetailJSON `json:"commit"`
	Parents []gitSHAJSON     `json:"parents"`
}

func renderCommitListItem(c ghdata.CachedGitCommit) commitListItemJSON {
	parents := make([]gitSHAJSON, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, gitSHAJSON{SHA: p})
	}
	return commitListItemJSON{
		SHA: c.SHA,
		Commit: commitDetailJSON{
			Author:    gitPersonJSON{Name: c.AuthorName, Email: c.AuthorEmail, Date: c.AuthorDate},
			Committer: gitPersonJSON{Name: c.CommitterName, Email: c.CommitterEmail, Date: c.CommitterDate},
			Message:   c.Message,
			Tree:      gitSHAJSON{SHA: c.TreeSHA},
		},
		Parents: parents,
	}
}

// upstreamCommitItem is the GitHub-shaped commit item as it appears in the
// commits LIST -- and, identically shaped, in a compare response's `commits`
// array (respcache_compare.go) -- with only the fields the model holds.
type upstreamCommitItem struct {
	SHA    string `json:"sha"`
	Commit struct {
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
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// toCachedGitCommit validates and converts one listed item into a git-commit
// row (shas lowercased). false = a shape the model cannot hold; callers pass
// the whole response through rather than store a hole.
func (item upstreamCommitItem) toCachedGitCommit(owner, repo string) (ghdata.CachedGitCommit, bool) {
	sha := strings.ToLower(item.SHA)
	if !isFullHexSHA(sha) || item.Commit.Tree.SHA == "" {
		return ghdata.CachedGitCommit{}, false
	}
	parents := make([]string, 0, len(item.Parents))
	for _, p := range item.Parents {
		parents = append(parents, strings.ToLower(p.SHA))
	}
	return ghdata.CachedGitCommit{
		Owner: owner, Repo: repo, SHA: sha, Message: item.Commit.Message,
		AuthorName: item.Commit.Author.Name, AuthorEmail: item.Commit.Author.Email, AuthorDate: item.Commit.Author.Date,
		CommitterName: item.Commit.Committer.Name, CommitterEmail: item.Commit.Committer.Email, CommitterDate: item.Commit.Committer.Date,
		TreeSHA: item.Commit.Tree.SHA, Parents: parents,
	}, true
}

// absorbCommitsList parses a /commits 200 array into git-commit rows in
// response order (an empty array -- a page past the end of history -- is a
// valid, cacheable answer). Reports false -- serve verbatim, store nothing --
// for any other status or any item the model cannot hold.
func absorbCommitsList(owner, repo string, status int, body []byte) ([]ghdata.CachedGitCommit, bool) {
	if status != http.StatusOK {
		return nil, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var raw []upstreamCommitItem
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return nil, false
	}
	commits := make([]ghdata.CachedGitCommit, 0, len(raw))
	for _, item := range raw {
		c, ok := item.toCachedGitCommit(owner, repo)
		if !ok {
			return nil, false
		}
		commits = append(commits, c)
	}
	return commits, true
}
