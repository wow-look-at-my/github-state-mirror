// The check-run annotations listing, GET .../check-runs/{id}/annotations.

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The check-run annotations listing, GET .../check-runs/{id}/annotations.

const (
	// annotationsSettledTTL bounds the annotations of a completed check run, which no longer change.
	annotationsSettledTTL = 24 * time.Hour

	// annotationsLiveTTL bounds the annotations of a run not known to be completed.
	annotationsLiveTTL = 2 * time.Minute

	annotationsDefaultPerPage = 30
	annotationsMaxCachedPage  = 10
)

func (h *handlers) cachedCheckRunAnnotations(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	id, err := strconv.ParseInt(chi.URLParam(r, "check_run_id"), 10, 64)
	if err != nil || id <= 0 {
		h.passthrough(w, r, PassPath)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	shape, ok := parsePagingShape(r.URL.Query(), annotationsDefaultPerPage, annotationsMaxCachedPage)
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	ref := strconv.FormatInt(id, 10)
	ttl := annotationsLiveTTL
	if h.checkRunCompleted(r, owner, repo, id) {
		ttl = annotationsSettledTTL
	}
	h.serveRepoAnswer(w, r, repoAnswerRoute{
		key:      ghdata.RepoAnswerKey{Owner: owner, Repo: repo, Kind: ghdata.RepoAnswerKindAnnotations, RefID: ref, Shape: shape},
		denyKind: denyKindAnnotations,
		resource: owner + "/" + repo + "/check-runs/" + ref + "/annotations?" + shape,
		absorb: func(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool) {
			a, ok := absorbAnnotations(resp, body, now.Add(ttl))
			a.Subject = ref
			return a, ok
		},
		refusals: []int{http.StatusNotFound},
		checks:   true,
	})
}

// checkRunCompleted reports whether the stored single check run says the run
// is completed. Unknown counts as not completed.
func (h *handlers) checkRunCompleted(r *http.Request, owner, repo string, id int64) bool {
	doc, ok, err := h.store.GetCachedCheckRun(r.Context(), owner, repo, id, time.Now())
	if err != nil || !ok {
		return false
	}
	var run struct {
		Status string `json:"status"`
	}
	return json.Unmarshal([]byte(doc), &run) == nil && run.Status == "completed"
}

// annotationJSON is one annotation; blob_href is a link and is dropped.
type annotationJSON struct {
	Path            string  `json:"path"`
	StartLine       int64   `json:"start_line"`
	EndLine         int64   `json:"end_line"`
	StartColumn     *int64  `json:"start_column"`
	EndColumn       *int64  `json:"end_column"`
	AnnotationLevel string  `json:"annotation_level"`
	Title           *string `json:"title"`
	Message         string  `json:"message"`
	RawDetails      *string `json:"raw_details"`
}

func absorbAnnotations(resp *http.Response, body []byte, expires time.Time) (ghdata.RepoAnswer, bool) {
	if resp.StatusCode != http.StatusOK {
		return ghdata.RepoAnswer{}, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return ghdata.RepoAnswer{}, false
	}
	var items []annotationJSON
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return ghdata.RepoAnswer{}, false
	}
	if items == nil {
		items = []annotationJSON{}
	}
	doc, err := marshalTrimmed(items)
	if err != nil {
		return ghdata.RepoAnswer{}, false
	}
	return ghdata.RepoAnswer{Status: http.StatusOK, Doc: string(doc), ExpiresAt: expires}, true
}
