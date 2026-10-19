package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The cached issue reads: GET /repos/{owner}/{repo}/issues/{number} and
// GET /repos/{owner}/{repo}/issues. see docs/cache/rest-routes.md

const (
	issuesDefaultPerPage = 30
	issuesMaxCachedPage  = 10
	// issuesMaxLabelsLen bounds the labels filter key component.
	issuesMaxLabelsLen = 255
)

func (h *handlers) cachedIssue(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	number, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
	if err != nil || number <= 0 {
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
	key := ghdata.IssueAnswerKey(owner, repo, number)
	h.serveRepoAnswer(w, r, repoAnswerRoute{
		key:      key,
		denyKind: denyKindIssue,
		resource: owner + "/" + repo + "/issues/" + key.RefID,
		absorb: func(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
			a, ok := absorbIssue(resp, body, now, number)
			a.Subject = key.RefID
			return a, ok
		},
	})
}

func absorbIssue(resp *http.Response, body []byte, now time.Time, number int64) (ghdata.RepoAnswer, bool) {
	switch resp.StatusCode {
	case http.StatusOK:
		doc, n, ok := ghdata.RenderIssueDoc(body)
		if !ok || n != number {
			return ghdata.RepoAnswer{}, false
		}
		return ghdata.RepoAnswer{Status: http.StatusOK, Doc: doc, ExpiresAt: now.Add(ghdata.IssueCacheTTL)}, true
	case http.StatusNotFound, http.StatusGone:
		return verdictAnswer(resp.StatusCode, body, now.Add(ghdata.IssueCacheTTL))
	}
	return ghdata.RepoAnswer{}, false
}

func (h *handlers) cachedIssuesList(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	shape, ok := parseIssuesListShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	h.serveRepoAnswer(w, r, repoAnswerRoute{
		key:      ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: ghdata.RepoAnswerKindIssuesList, Shape: shape},
		denyKind: denyKindIssuesList,
		resource: owner + "/" + repo + "/issues?" + shape,
		absorb:   absorbIssuesList,
	})
}

// parseIssuesListShape canonicalizes the modeled query: state, labels and
// paging. Defaults are written out, so an absent value and its default
// share a row.
func parseIssuesListShape(q url.Values) (string, bool) {
	shape := url.Values{
		"state":    {"open"},
		"per_page": {strconv.Itoa(issuesDefaultPerPage)},
		"page":     {"1"},
	}
	for key, vals := range q {
		if len(vals) != 1 {
			return "", false
		}
		v := vals[0]
		switch key {
		case "state":
			if v != "open" && v != "closed" && v != "all" {
				return "", false
			}
		case "per_page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return "", false
			}
			v = strconv.Itoa(n)
		case "page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > issuesMaxCachedPage {
				return "", false
			}
			v = strconv.Itoa(n)
		case "labels":
			if v == "" || len(v) > issuesMaxLabelsLen || strings.ContainsFunc(v, isControlRune) {
				return "", false
			}
		default:
			return "", false
		}
		shape.Set(key, v)
	}
	return shape.Encode(), true
}

func absorbIssuesList(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
	switch resp.StatusCode {
	case http.StatusOK:
		doc, ok := ghdata.RenderIssuesListDoc(body)
		if !ok {
			return ghdata.RepoAnswer{}, false
		}
		return ghdata.RepoAnswer{Status: http.StatusOK, Doc: doc, ExpiresAt: now.Add(ghdata.IssuesListCacheTTL)}, true
	case http.StatusNotFound, http.StatusGone:
		return verdictAnswer(resp.StatusCode, body, now.Add(ghdata.IssuesListCacheTTL))
	}
	return ghdata.RepoAnswer{}, false
}
