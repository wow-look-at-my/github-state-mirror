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

// GET /repos/{owner}/{repo}/compare/{basehead}, tier 2 of the cache contract.

// compareCacheTTL is the backstop for a missed push delivery.
const compareCacheTTL = 24 * time.Hour

// compareBaseheadCacheable rejects a cross-fork owner:branch component (its
// pushes never reach this repo's flush) and anything without a three-dot.
func compareBaseheadCacheable(basehead string) bool {
	if strings.Contains(basehead, ":") {
		return false
	}
	i := strings.Index(basehead, "...")
	if i < 0 {
		return false
	}
	return basehead[:i] != "" && basehead[i+3:] != ""
}

// cachedCompare serves a three-dot comparison from absorbed state, fetching
// and absorbing on a miss.
func (h *handlers) cachedCompare(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	basehead := chi.URLParam(r, "*")

	// Only the bare default-JSON, no-query shape is modeled.
	if !acceptsDefaultJSON(r) || r.URL.RawQuery != "" || !compareBaseheadCacheable(basehead) {
		h.passthrough(w, r, shapeReason(r, compareBaseheadCacheable(basehead)))
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindCompare, owner+"/"+repo+"/compare/"+basehead); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if c, ok, err := h.store.GetCachedCompare(r.Context(), owner, repo, basehead, now); err != nil {
		slog.Warn("compare cache read failed", "owner", owner, "repo", repo, "basehead", basehead, "error", err)
	} else if ok {
		// 200 (a real comparison) or 404 (an unknown-ref verdict).
		h.serveCompare(w, r, c.Status, c.Doc, true)
		return
	}

	// Miss: fetch from GitHub with the caller's own credentials.
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	status := http.StatusOK
	absorbedDoc, absorbed := absorbCompare(owner, repo, resp.StatusCode, body)
	doc, commits, baseTip := absorbedDoc.Doc, absorbedDoc.Commits, absorbedDoc.BaseTipSHA
	if !absorbed && !overflow && resp.StatusCode == http.StatusNotFound {
		// The unknown-ref verdict is absorbed too; see docs/cache/rest-routes.md.
		if doc404, mErr := marshalTrimmed(notFoundJSON{Message: upstreamErrorMessage(body), Status: "404"}); mErr == nil {
			doc, commits, absorbed, status = string(doc404), nil, true, http.StatusNotFound
		}
	}
	if overflow || !absorbed {
		// 5xx and unexpected shapes: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	// compareBaseheadCacheable already guarantees the split cannot fail.
	baseRef, headRef, _ := strings.Cut(basehead, "...")
	if err := h.store.PutCachedCompare(r.Context(), ghdata.CachedCompare{
		Owner: owner, Repo: repo, Basehead: basehead,
		BaseRef: baseRef, HeadRef: headRef, BaseTipSha: baseTip,
		Status: status, Doc: doc,
	}, commits, now, compareCacheTTL); err != nil {
		slog.Warn("compare cache write failed", "owner", owner, "repo", repo, "basehead", basehead, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	h.serveCompare(w, r, status, doc, false)
}

// serveCompare writes the doc under the status it absorbed; hit and miss
// serve identical rendered bytes.
func (h *handlers) serveCompare(w http.ResponseWriter, r *http.Request, status int, doc string, hit bool) {
	if hit {
		if status == http.StatusOK {
			h.reqlog.observe(r, DispHit)
		} else {
			h.reqlog.observeStatus(r, DispHit, status)
		}
	}
	writeRebuilt(w, status, []byte(doc), hit)
}

// compareFileJSON is one trimmed comparison file entry. see docs/cache/rest-routes.md
type compareFileJSON struct {
	Filename         string `json:"filename"`
	Status           string `json:"status"`
	Additions        int64  `json:"additions"`
	Deletions        int64  `json:"deletions"`
	Changes          int64  `json:"changes"`
	PreviousFilename string `json:"previous_filename,omitempty"`
}

// Files is a POINTER: its presence is load-bearing (absent means "unknown,
// fail open"), so nil must not collapse into an empty array.
type compareDocJSON struct {
	Status          string               `json:"status"`
	AheadBy         int64                `json:"ahead_by"`
	BehindBy        int64                `json:"behind_by"`
	TotalCommits    int64                `json:"total_commits"`
	MergeBaseCommit *gitSHAJSON          `json:"merge_base_commit,omitempty"`
	Commits         []commitListItemJSON `json:"commits"`
	Files           *[]compareFileJSON   `json:"files,omitempty"`
}

// absorbedCompare is what one comparison contributes to the cache.
type absorbedCompare struct {
	Doc        string
	Commits    []ghdata.CachedGitCommit
	BaseTipSHA string // base_commit.sha, "" when the body did not state one
}

// absorbCompare reports false for any shape the model cannot hold; the
// caller then serves the body verbatim and stores nothing.
func absorbCompare(owner, repo string, status int, body []byte) (absorbedCompare, bool) {
	if status != http.StatusOK {
		return absorbedCompare{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return absorbedCompare{}, false
	}
	var raw struct {
		Status       string `json:"status"`
		AheadBy      int64  `json:"ahead_by"`
		BehindBy     int64  `json:"behind_by"`
		TotalCommits int64  `json:"total_commits"`
		BaseCommit   *struct {
			SHA string `json:"sha"`
		} `json:"base_commit"`
		MergeBaseCommit *struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
		Commits []upstreamCommitItem `json:"commits"`
		Files   *[]struct {
			Filename         string `json:"filename"`
			Status           string `json:"status"`
			Additions        int64  `json:"additions"`
			Deletions        int64  `json:"deletions"`
			Changes          int64  `json:"changes"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.Status == "" {
		return absorbedCompare{}, false
	}

	doc := compareDocJSON{
		Status:       raw.Status,
		AheadBy:      raw.AheadBy,
		BehindBy:     raw.BehindBy,
		TotalCommits: raw.TotalCommits,
		Commits:      make([]commitListItemJSON, 0, len(raw.Commits)),
	}
	if raw.MergeBaseCommit != nil {
		sha := strings.ToLower(raw.MergeBaseCommit.SHA)
		if !isFullHexSHA(sha) {
			return absorbedCompare{}, false
		}
		doc.MergeBaseCommit = &gitSHAJSON{SHA: sha}
	}
	// Row metadata only: kept off the rendered document, whose shape is pinned.
	baseTip := ""
	if raw.BaseCommit != nil {
		if sha := strings.ToLower(raw.BaseCommit.SHA); isFullHexSHA(sha) {
			baseTip = sha
		}
	}
	commits := make([]ghdata.CachedGitCommit, 0, len(raw.Commits))
	for _, item := range raw.Commits {
		c, ok := item.toCachedGitCommit(owner, repo)
		if !ok {
			return absorbedCompare{}, false
		}
		commits = append(commits, c)
		doc.Commits = append(doc.Commits, renderCommitListItem(c))
	}
	if raw.Files != nil {
		files := make([]compareFileJSON, 0, len(*raw.Files))
		for _, f := range *raw.Files {
			files = append(files, compareFileJSON{
				Filename: f.Filename, Status: f.Status,
				Additions: f.Additions, Deletions: f.Deletions, Changes: f.Changes,
				PreviousFilename: f.PreviousFilename,
			})
		}
		doc.Files = &files
	}

	rendered, err := marshalTrimmed(doc)
	if err != nil {
		return absorbedCompare{}, false
	}
	return absorbedCompare{Doc: string(rendered), Commits: commits, BaseTipSHA: baseTip}, true
}
