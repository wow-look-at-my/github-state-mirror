package api

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The cached redirects: Actions log downloads. GitHub
// answers each with a 302 to a location that stops working on its own clock,
// so a stored redirect lives only until that clock says, less a margin.

const (
	// redirectExpiryMargin is taken off a signed location's stated expiry, so
	// a client that follows a served redirect still has time to use it.
	redirectExpiryMargin = 15 * time.Second


	// logsGoneTTL bounds the 410 "logs expired" verdict. Expired logs do not
	// come back, so this only bounds a re-run that writes new ones.
	logsGoneTTL = 24 * time.Hour

)

// cachedRunLogs serves GET /repos/{owner}/{repo}/actions/runs/{run_id}/logs.
func (h *handlers) cachedRunLogs(w http.ResponseWriter, r *http.Request) {
	h.serveLogs(w, r, "run_id", ghdata.RepoAnswerKindRunLogs, denyKindRunLogs)
}

// cachedJobLogs serves GET /repos/{owner}/{repo}/actions/jobs/{job_id}/logs.
func (h *handlers) cachedJobLogs(w http.ResponseWriter, r *http.Request) {
	h.serveLogs(w, r, "job_id", ghdata.RepoAnswerKindJobLogs, denyKindJobLogs)
}

func (h *handlers) serveLogs(w http.ResponseWriter, r *http.Request, param, kind, denyKind string) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	id, err := strconv.ParseInt(chi.URLParam(r, param), 10, 64)
	if err != nil || id <= 0 {
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
	ref := strconv.FormatInt(id, 10)
	h.serveRepoAnswer(w, r, repoAnswerRoute{
		key:        ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: kind, RefID: ref},
		denyKind:   denyKind,
		resource:   owner + "/" + repo + "/" + kind + "/" + ref,
		absorb:     absorbLogs,
		refusals:   []int{http.StatusNotFound},
		noRedirect: true,
	})
}

// absorbLogs stores a redirect until its signed location expires, and the
// 410 "logs expired" verdict. A redirect with no expiry it can read is
// relayed unstored.
func absorbLogs(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
	switch resp.StatusCode {
	case http.StatusFound:
		loc := resp.Header.Get("Location")
		exp, ok := signedURLExpiry(loc)
		if !ok {
			return ghdata.RepoAnswer{}, false
		}
		return redirectAnswer(loc, exp.Add(-redirectExpiryMargin))
	case http.StatusGone:
		return verdictAnswer(resp.StatusCode, body, now.Add(logsGoneTTL))
	}
	return ghdata.RepoAnswer{}, false
}

// redirectAnswer stores a 302 to loc until expires.
func redirectAnswer(loc string, expires time.Time) (ghdata.RepoAnswer, bool) {
	if loc == "" {
		return ghdata.RepoAnswer{}, false
	}
	doc, err := marshalTrimmed(redirectJSON{Location: loc})
	if err != nil {
		return ghdata.RepoAnswer{}, false
	}
	return ghdata.RepoAnswer{Status: http.StatusFound, Doc: string(doc), ExpiresAt: expires}, true
}

// signedURLExpiry reads the expiry a signed location states: an Azure SAS
// "se", an S3 "X-Amz-Date" plus "X-Amz-Expires", or a unix "Expires".
func signedURLExpiry(loc string) (time.Time, bool) {
	u, err := url.Parse(loc)
	if err != nil || !u.IsAbs() {
		return time.Time{}, false
	}
	q := u.Query()
	if se := q.Get("se"); se != "" {
		t, err := time.Parse(time.RFC3339, se)
		return t, err == nil
	}
	if date, secs := q.Get("X-Amz-Date"), q.Get("X-Amz-Expires"); date != "" && secs != "" {
		t, err := time.Parse("20060102T150405Z", date)
		n, nerr := strconv.Atoi(secs)
		if err != nil || nerr != nil || n <= 0 {
			return time.Time{}, false
		}
		return t.Add(time.Duration(n) * time.Second), true
	}
	if e := q.Get("Expires"); e != "" {
		n, err := strconv.ParseInt(e, 10, 64)
		if err != nil || n <= 0 {
			return time.Time{}, false
		}
		return time.Unix(n, 0), true
	}
	return time.Time{}, false
}
