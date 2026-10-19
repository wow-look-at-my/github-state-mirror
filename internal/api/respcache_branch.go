package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The single-branch read, GET /repos/{owner}/{repo}/branches/{branch}.

const (
	// branchTTL bounds a branch answer. A push or a ref delivery flushes it sooner.
	branchTTL = 5 * time.Minute

	// branchAbsentTTL bounds the absent-branch verdict. A create flushes it sooner.
	branchAbsentTTL = 10 * time.Minute

	branchMaxNameLen = 255
)

// cachedBranch serves one branch. The name may carry slashes, so the route is
// a wildcard. chi routes on the raw path, so an encoded slash is decoded here.
func (h *handlers) cachedBranch(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	branch, err := url.PathUnescape(chi.URLParam(r, "*"))
	if err != nil || branch == "" || len(branch) > branchMaxNameLen || strings.ContainsFunc(branch, isControlRune) {
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
		key:      ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: ghdata.RepoAnswerKindBranch, RefID: branch},
		denyKind: denyKindBranch,
		resource: owner + "/" + repo + "/branches/" + branch,
		absorb: func(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
			a, ok := absorbBranch(resp.StatusCode, body, now)
			a.Subject = branch
			return a, ok
		},
	})
}

// branchJSON is the trimmed single-branch answer.
type branchJSON struct {
	Name      string           `json:"name"`
	Commit    branchCommitJSON `json:"commit"`
	Protected bool             `json:"protected"`
}

type branchCommitJSON struct {
	SHA     string          `json:"sha"`
	Commit  branchGitCommit `json:"commit"`
	Parents []gitSHAJSON    `json:"parents"`
}

type branchGitCommit struct {
	Author    *branchSignature `json:"author"`
	Committer *branchSignature `json:"committer"`
	Message   string           `json:"message"`
	Tree      gitSHAJSON       `json:"tree"`
}

type branchSignature struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

func absorbBranch(status int, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
	if status == http.StatusNotFound {
		return verdictAnswer(status, body, now.Add(branchAbsentTTL))
	}
	if status != http.StatusOK {
		return ghdata.RepoAnswer{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ghdata.RepoAnswer{}, false
	}
	var raw struct {
		Name   string `json:"name"`
		Commit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Author    *branchSignature `json:"author"`
				Committer *branchSignature `json:"committer"`
				Message   string           `json:"message"`
				Tree      gitSHAJSON       `json:"tree"`
			} `json:"commit"`
			Parents []gitSHAJSON `json:"parents"`
		} `json:"commit"`
		Protected bool `json:"protected"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return ghdata.RepoAnswer{}, false
	}
	sha := strings.ToLower(raw.Commit.SHA)
	if raw.Name == "" || !isFullHexSHA(sha) {
		return ghdata.RepoAnswer{}, false
	}
	parents := make([]gitSHAJSON, 0, len(raw.Commit.Parents))
	for _, p := range raw.Commit.Parents {
		parents = append(parents, gitSHAJSON{SHA: strings.ToLower(p.SHA)})
	}
	doc, err := marshalTrimmed(branchJSON{
		Name: raw.Name,
		Commit: branchCommitJSON{
			SHA: sha,
			Commit: branchGitCommit{
				Author: raw.Commit.Commit.Author, Committer: raw.Commit.Commit.Committer,
				Message: raw.Commit.Commit.Message, Tree: gitSHAJSON{SHA: strings.ToLower(raw.Commit.Commit.Tree.SHA)},
			},
			Parents: parents,
		},
		Protected: raw.Protected,
	})
	if err != nil {
		return ghdata.RepoAnswer{}, false
	}
	return ghdata.RepoAnswer{Status: http.StatusOK, Doc: string(doc), ExpiresAt: now.Add(branchTTL)}, true
}
