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

// This file implements the cached compare route (tier 2 of the cache
// contract, like respcache.go):
//
//	GET /repos/{owner}/{repo}/compare/{basehead}
//
// pr-minder's auto_open_pr catch-up and close-empty gates run a three-dot
// base...head comparison per branch, and the pr-minder-reconcile hook repeats
// that over a fleet slice every 5 minutes -- the passthrough flood (~90% of
// the request log) that motivated this route. HISTORY WARNING: an earlier
// /compare cache was REMOVED because its trim dropped the `files` array,
// breaking pr-minder's empty-PR gate (changed_files = files.length; an ABSENT
// array means unknown -> fail open, an EMPTY one means a 0-diff branch ->
// close/skip). This rebuild therefore preserves files presence/absence and
// per-file counts exactly; only URL fields, user-object clutter, and the
// unread per-file `patch` are dropped.
//
// The route is greedy (`compare/*`): a basehead routinely carries slashes in
// its branch names (claude/foo...release/v1). The whole trimmed document is
// stored per exact basehead; the compare's commits are also absorbed into the
// global git_commits_cache rows (synergy with the single-commit and
// commits-list routes). A comparison depends on both refs' tips, so
// push/repository webhooks flush all of a repo's rows, with a 24h TTL
// backstop.

// compareCacheTTL bounds how long a MISSED push delivery could leave a stale
// comparison being served. Webhooks flush sooner; this is the backstop.
const compareCacheTTL = 24 * time.Hour

// compareBaseheadCacheable reports whether a basehead path tail is a shape
// the cache models: a three-dot base...head with both sides non-empty and no
// cross-fork owner:branch component. A colon means the head (or base) lives
// in ANOTHER repo, whose pushes never reach this repo's webhook flush -- a
// cached row could serve a stale comparison forever, so that form always
// passes through. No three-dot separator (including GitHub's unsupported
// two-dot form) is not a shape we model either.
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

	// Only the default JSON representation with NO query params is modeled:
	// the .diff/.patch media types are entirely different response shapes,
	// and ?per_page/?page change which commits the body carries. The
	// consumers (pr-minder + the reconcile hook) send exactly this bare
	// shape.
	if !acceptsDefaultJSON(r) || r.URL.RawQuery != "" || !compareBaseheadCacheable(basehead) {
		h.ghProxy.ServeHTTP(w, r)
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
		h.serveCompare(w, r, c.Doc, true)
		return
	}

	// Miss: fetch from GitHub with the caller's own credentials.
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	doc, commits, absorbed := absorbCompare(owner, repo, resp.StatusCode, body)
	if overflow || !absorbed {
		// Includes 404 (unknown ref -- it can be pushed later) and 5xx:
		// relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	// The route guard above (compareBaseheadCacheable) guarantees the
	// three-dot form with both sides non-empty, so the split cannot fail;
	// the two sides feed the per-ref webhook invalidation. Status is always
	// 200 here -- stage-2 also absorbs 404 "unknown ref" answers as
	// expiring miss markers.
	baseRef, headRef, _ := strings.Cut(basehead, "...")
	if err := h.store.PutCachedCompare(r.Context(), ghdata.CachedCompare{
		Owner: owner, Repo: repo, Basehead: basehead,
		BaseRef: baseRef, HeadRef: headRef, Status: http.StatusOK,
		Doc: doc,
	}, commits, now, compareCacheTTL); err != nil {
		slog.Warn("compare cache write failed", "owner", owner, "repo", repo, "basehead", basehead, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispMiss, resp.StatusCode)
	h.serveCompare(w, r, doc, false)
}

// serveCompare writes the trimmed compare document. The doc is rendered once
// at absorb time and stored verbatim, so hit and miss serve identical bytes.
func (h *handlers) serveCompare(w http.ResponseWriter, r *http.Request, doc string, hit bool) {
	if hit {
		h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, DispHit)
	}
	writeRebuilt(w, http.StatusOK, []byte(doc), hit)
}

// compareFileJSON is one trimmed entry of the comparison's files array. The
// per-file counts are kept in full -- files/changed_files is THE field whose
// loss broke pr-minder's empty-PR gate the last time /compare was cached --
// while blob_url/raw_url/contents_url and the unread `patch` are dropped.
type compareFileJSON struct {
	Filename         string `json:"filename"`
	Status           string `json:"status"`
	Additions        int64  `json:"additions"`
	Deletions        int64  `json:"deletions"`
	Changes          int64  `json:"changes"`
	PreviousFilename string `json:"previous_filename,omitempty"`
}

// compareDocJSON is the trimmed rebuild of a comparison: a superset of what
// pr-minder + the pr-minder-reconcile hook read (ahead_by, behind_by, and the
// files array's presence + length). merge_base_commit is trimmed to its sha;
// commits reuse the commits-list item shape. Files is a POINTER because its
// presence is load-bearing: GitHub omits the array on an oversized response
// and consumers read that as "unknown, fail open" -- the rebuild must
// preserve absent-vs-empty exactly.
type compareDocJSON struct {
	Status          string               `json:"status"`
	AheadBy         int64                `json:"ahead_by"`
	BehindBy        int64                `json:"behind_by"`
	TotalCommits    int64                `json:"total_commits"`
	MergeBaseCommit *gitSHAJSON          `json:"merge_base_commit,omitempty"`
	Commits         []commitListItemJSON `json:"commits"`
	Files           *[]compareFileJSON   `json:"files,omitempty"`
}

// absorbCompare parses a compare 200 into the trimmed document (rendered once
// here; hits serve the stored bytes) plus the comparison's commits as
// git-commit rows. Reports false -- serve verbatim, store nothing -- for any
// other status or any shape the model cannot hold.
func absorbCompare(owner, repo string, status int, body []byte) (string, []ghdata.CachedGitCommit, bool) {
	if status != http.StatusOK {
		return "", nil, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", nil, false
	}
	var raw struct {
		Status          string `json:"status"`
		AheadBy         int64  `json:"ahead_by"`
		BehindBy        int64  `json:"behind_by"`
		TotalCommits    int64  `json:"total_commits"`
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
		return "", nil, false
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
			return "", nil, false
		}
		doc.MergeBaseCommit = &gitSHAJSON{SHA: sha}
	}
	commits := make([]ghdata.CachedGitCommit, 0, len(raw.Commits))
	for _, item := range raw.Commits {
		c, ok := item.toCachedGitCommit(owner, repo)
		if !ok {
			return "", nil, false
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
		return "", nil, false
	}
	return string(rendered), commits, true
}
