package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The commit read, GET /repos/{owner}/{repo}/commits/{ref}, and the branches
// whose tip a commit is, .../commits/{sha}/branches-where-head.

const (
	// commitImmutableTTL bounds a commit asked for by its full sha.
	commitImmutableTTL = 30 * 24 * time.Hour

	// commitRefTTL bounds a commit asked for by a branch name or a short sha.
	commitRefTTL = 5 * time.Minute

	commitVerdictTTL = 10 * time.Minute

	// commitMaxDocBytes caps a stored commit: files carry unbounded patches.
	commitMaxDocBytes = 1 << 20

	// commitMaxRefLen bounds the ref key component.
	commitMaxRefLen = 255
)

func (h *handlers) cachedCommit(w http.ResponseWriter, r *http.Request, ref string) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	if len(ref) > commitMaxRefLen || strings.ContainsFunc(ref, isControlRune) {
		h.passthrough(w, r, PassPath)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	if len(r.URL.Query()) > 0 {
		// The files listing paginates; no page shape is modeled.
		h.passthrough(w, r, PassQuery)
		return
	}
	ttl, subject := commitRefTTL, ref
	if isFullHexSHA(strings.ToLower(ref)) {
		ttl, subject = commitImmutableTTL, ""
	}
	h.serveRepoAnswer(w, r, repoAnswerRoute{
		key:      ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: ghdata.RepoAnswerKindCommit, RefID: ref},
		denyKind: denyKindCommit,
		resource: owner + "/" + repo + "/commits/" + ref,
		absorb: func(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
			a, ok := absorbCommit(resp, body, now, ttl)
			a.Subject = subject
			return a, ok
		},
		refusals: []int{http.StatusNotFound},
	})
}

func (h *handlers) cachedBranchesWhereHead(w http.ResponseWriter, r *http.Request, sha string) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	sha = strings.ToLower(sha)
	if !isFullHexSHA(sha) {
		h.passthrough(w, r, PassPath)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	if len(r.URL.Query()) > 0 {
		h.passthrough(w, r, PassQuery)
		return
	}
	h.serveRepoAnswer(w, r, repoAnswerRoute{
		key:      ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: ghdata.RepoAnswerKindBranchesWhereHead, RefID: sha},
		denyKind: denyKindBranchesWhereHead,
		resource: owner + "/" + repo + "/commits/" + sha + "/branches-where-head",
		absorb: func(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
			a, ok := absorbBranchesWhereHead(resp, body, now)
			a.Subject = sha
			return a, ok
		},
		refusals: []int{http.StatusNotFound},
	})
}

// gitPersonJSON is a git author or committer.
type gitPersonJSON struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

// commitVerificationJSON is the signature verification. Every field but
// verified is nullable.
type commitVerificationJSON struct {
	Verified   bool    `json:"verified"`
	Reason     string  `json:"reason"`
	Signature  *string `json:"signature"`
	Payload    *string `json:"payload"`
	VerifiedAt *string `json:"verified_at"`
}

// commitAccountJSON is the GitHub account behind an author or committer.
type commitAccountJSON struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

type commitShaJSON struct {
	SHA string `json:"sha"`
}

type commitStatsJSON struct {
	Additions int64 `json:"additions"`
	Deletions int64 `json:"deletions"`
	Total     int64 `json:"total"`
}

// commitFileJSON keeps the PRESENCE of patch and previous_filename exactly.
type commitFileJSON struct {
	SHA              string  `json:"sha"`
	Filename         string  `json:"filename"`
	Status           string  `json:"status"`
	Additions        int64   `json:"additions"`
	Deletions        int64   `json:"deletions"`
	Changes          int64   `json:"changes"`
	Patch            *string `json:"patch,omitempty"`
	PreviousFilename *string `json:"previous_filename,omitempty"`
}

type commitGitJSON struct {
	Author       *gitPersonJSON          `json:"author"`
	Committer    *gitPersonJSON          `json:"committer"`
	Message      string                  `json:"message"`
	Tree         commitShaJSON           `json:"tree"`
	CommentCount int64                   `json:"comment_count"`
	Verification *commitVerificationJSON `json:"verification"`
}

// commitJSON is the trimmed single commit; every URL field is dropped.
type commitJSON struct {
	SHA       string             `json:"sha"`
	NodeID    string             `json:"node_id"`
	Commit    commitGitJSON      `json:"commit"`
	Author    *commitAccountJSON `json:"author"`
	Committer *commitAccountJSON `json:"committer"`
	Parents   []commitShaJSON    `json:"parents"`
	Stats     *commitStatsJSON   `json:"stats"`
	Files     []commitFileJSON   `json:"files"`
}

func absorbCommit(resp *http.Response, body []byte, now time.Time, ttl time.Duration) (ghdata.RepoAnswer, bool) {
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return verdictAnswer(resp.StatusCode, body, now.Add(commitVerdictTTL))
	}
	if resp.StatusCode != http.StatusOK {
		return ghdata.RepoAnswer{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ghdata.RepoAnswer{}, false
	}
	var c commitJSON
	if err := json.Unmarshal(trimmed, &c); err != nil || !isFullHexSHA(c.SHA) {
		return ghdata.RepoAnswer{}, false
	}
	if c.Parents == nil {
		c.Parents = []commitShaJSON{}
	}
	if c.Files == nil {
		c.Files = []commitFileJSON{}
	}
	doc, err := marshalTrimmed(c)
	if err != nil || len(doc) > commitMaxDocBytes {
		return ghdata.RepoAnswer{}, false
	}
	return ghdata.RepoAnswer{Status: http.StatusOK, Doc: string(doc), ExpiresAt: now.Add(ttl)}, true
}

func absorbBranchesWhereHead(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
	if resp.StatusCode != http.StatusOK {
		return ghdata.RepoAnswer{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return ghdata.RepoAnswer{}, false
	}
	var items []ghdata.StoredBranchHead
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return ghdata.RepoAnswer{}, false
	}
	for _, item := range items {
		if item.Name == "" || !isFullHexSHA(item.Commit.SHA) {
			return ghdata.RepoAnswer{}, false
		}
	}
	if items == nil {
		items = []ghdata.StoredBranchHead{}
	}
	doc, err := marshalTrimmed(items)
	if err != nil {
		return ghdata.RepoAnswer{}, false
	}
	return ghdata.RepoAnswer{Status: http.StatusOK, Doc: string(doc), ExpiresAt: now.Add(ghdata.BranchesWhereHeadTTL)}, true
}
