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

// Implements the cached commits LIST route, GET /repos/{owner}/{repo}/commits (tier 2).
// See docs/cache/rest-routes.md for the key shape, hit gate, and invalidation.

const (
	// commitsListCacheTTL is the backstop for a missed push delivery.
	commitsListCacheTTL = 24 * time.Hour

	// commitsDefaultPerPage is GitHub's default when per_page is absent.
	commitsDefaultPerPage = 30

	// commitsMaxCachedPage caps which pages are modeled; deeper pages pass through.
	commitsMaxCachedPage = 10
)

// commitsListShape is a parsed, cacheable /commits query.
type commitsListShape struct {
	refParam string // raw ?sha= value ('' = default branch)
	perPage  int
	page     int
}

// parseCommitsListShape reports whether the cache models this /commits query.
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
		h.passthrough(w, r, PassAccept)
		return
	}
	shape, ok := parseCommitsListShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
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
		// 404 (unknown ref), 409 (empty repo), and 5xx relay unstored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedCommitsList(r.Context(), owner, repo, shape.refParam, shape.perPage, shape.page, commits, now, commitsListCacheTTL); err != nil {
		slog.Warn("commits list absorb failed", "owner", owner, "repo", repo, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
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
		h.reqlog.observe(r, DispHit)
	}
	writeRebuilt(w, http.StatusOK, body, hit)
}

// commitDetailJSON nests the commit identities under `commit`, unlike the git-commit route's top-level shape.
type commitDetailJSON struct {
	Author    gitPersonJSON `json:"author"`
	Committer gitPersonJSON `json:"committer"`
	Message   string        `json:"message"`
	Tree      gitSHAJSON    `json:"tree"`
}

// commitListItemJSON is the trimmed rebuild of one commits-list item; see docs/cache/rest-routes.md for the dropped fields.
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
