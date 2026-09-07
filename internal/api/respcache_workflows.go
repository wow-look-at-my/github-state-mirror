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

// The cached Actions WORKFLOW definitions (tier 2 of the cache contract):
//
//	GET /repos/{owner}/{repo}/actions/workflows[?page=&per_page=]
//	GET /repos/{owner}/{repo}/actions/workflows/{id}
//
// A workflow definition is not a run. It changes only when a push edits
// .github/workflows, so both grains share one table and one repo-wide flush.
// see docs/cache/rest-routes.md

const (
	// GitHub's default per_page for the workflows listing.
	workflowsDefaultPerPage = 30

	// Caps modeled pages; deeper pagination passes through.
	workflowsMaxCachedPage = 10

	// Bounds the requested id key; an unbounded key component is a cardinality footgun.
	workflowsMaxIDLen = 255
)

// cachedWorkflowsList serves a repo's workflow listing from a stored snapshot.
func (h *handlers) cachedWorkflowsList(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))

	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	perPage, page, ok := parseWorkflowsShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	key := ghdata.WorkflowsKey{
		Owner: owner, Repo: repo, Kind: ghdata.WorkflowsKindList,
		PerPage: perPage, Page: page,
	}
	h.serveWorkflows(w, r, key, absorbWorkflowsList)
}

// cachedWorkflow serves one workflow definition. The id is whatever the caller
// asked with -- a numeric id or a file name -- and each spelling keys its own
// row, since resolving one to the other needs the very answer this caches.
func (h *handlers) cachedWorkflow(w http.ResponseWriter, r *http.Request) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))
	id := chi.URLParam(r, "id")

	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	if len(r.URL.Query()) > 0 {
		// The endpoint takes no parameters, so any is unmodeled by definition.
		h.passthrough(w, r, PassQuery)
		return
	}
	if id == "" || len(id) > workflowsMaxIDLen || strings.ContainsFunc(id, isControlRune) {
		h.passthrough(w, r, PassPath)
		return
	}
	key := ghdata.WorkflowsKey{
		Owner: owner, Repo: repo, Kind: ghdata.WorkflowsKindSingle, RefID: id,
	}
	h.serveWorkflows(w, r, key, absorbWorkflow)
}

// serveWorkflows is the shared read path for both grains: reveal, hit, fetch,
// absorb, rebuild. absorb renders the document, so a hit and a miss serve
// identical bytes.
func (h *handlers) serveWorkflows(w http.ResponseWriter, r *http.Request, key ghdata.WorkflowsKey, absorb func(int, []byte) (string, bool)) {
	switch outcome, verdict, cached := h.reveal(r, key.Owner, key.Repo, denyKindWorkflows, workflowsResourceKey(key)); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if doc, ok, err := h.store.GetCachedWorkflows(r.Context(), key, now); err != nil {
		slog.Warn("workflows cache read failed", "owner", key.Owner, "repo", key.Repo, "kind", key.Kind, "id", key.RefID, "error", err)
	} else if ok {
		h.reqlog.observe(r, DispHit)
		writeRebuilt(w, http.StatusOK, []byte(doc), true)
		return
	}

	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	doc, absorbed := absorb(resp.StatusCode, body)
	if overflow || !absorbed {
		// A 404 (deleted workflow), a 5xx, and any unmodeled shape relay verbatim and are never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedWorkflows(r.Context(), key, doc, now, ghdata.WorkflowsCacheTTL); err != nil {
		slog.Warn("workflows cache write failed", "owner", key.Owner, "repo", key.Repo, "kind", key.Kind, "id", key.RefID, "error", err)
	}
	h.refreshGrantOn2xx(r, key.Owner, key.Repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// workflowsResourceKey names the resource for the deny cache, at the grain the
// request asked at.
func workflowsResourceKey(key ghdata.WorkflowsKey) string {
	k := key.Owner + "/" + key.Repo + "/actions/workflows"
	if key.Kind == ghdata.WorkflowsKindSingle {
		return k + "/" + key.RefID
	}
	return k + "?per_page=" + strconv.FormatInt(key.PerPage, 10) + "&page=" + strconv.FormatInt(key.Page, 10)
}

// parseWorkflowsShape reports the modeled listing shape: paging only, which is
// every parameter this endpoint documents.
func parseWorkflowsShape(q url.Values) (perPage, page int64, ok bool) {
	perPage, page = workflowsDefaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return 0, 0, false
		}
		n, err := strconv.ParseInt(vals[0], 10, 64)
		switch key {
		case "per_page":
			if err != nil || n < 1 || n > 100 {
				return 0, 0, false
			}
			perPage = n
		case "page":
			if err != nil || n < 1 || n > workflowsMaxCachedPage {
				return 0, 0, false
			}
			page = n
		default:
			return 0, 0, false
		}
	}
	return perPage, page, true
}

// workflowJSON is the trimmed rebuild of a workflow definition. url and
// badge_url are dropped as the no-URL-keys invariant requires; html_url stays,
// the workflow-runs precedent, because it is the only handle a consumer has
// for linking a workflow back to GitHub.
type workflowJSON struct {
	ID        int64  `json:"id"`
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	HTMLURL   string `json:"html_url"`
}

// workflowsListJSON is the trimmed rebuild of the listing.
type workflowsListJSON struct {
	TotalCount int64          `json:"total_count"`
	Workflows  []workflowJSON `json:"workflows"`
}

// rawWorkflow is the subset of GitHub's workflow object the model holds.
type rawWorkflow struct {
	ID        int64  `json:"id"`
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	HTMLURL   string `json:"html_url"`
}

// trim reports the rebuild, and false when the answer is not the workflow
// object this route holds: an id and a path are what identify one.
func (raw rawWorkflow) trim() (workflowJSON, bool) {
	if raw.ID <= 0 || raw.Path == "" {
		return workflowJSON{}, false
	}
	return workflowJSON(raw), true
}

// absorbWorkflow parses a single-workflow 200 into the rendered document.
func absorbWorkflow(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false
	}
	var raw rawWorkflow
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return "", false
	}
	out, ok := raw.trim()
	if !ok {
		return "", false
	}
	rendered, err := marshalTrimmed(out)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}

// absorbWorkflowsList parses a listing 200 into the rendered document.
// total_count and the workflows array must both be PRESENT; an empty array
// with a zero count is a valid, cacheable "this repo has no workflows".
func absorbWorkflowsList(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false
	}
	var raw struct {
		TotalCount *int64         `json:"total_count"`
		Workflows  *[]rawWorkflow `json:"workflows"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.TotalCount == nil || raw.Workflows == nil {
		return "", false
	}
	out := workflowsListJSON{TotalCount: *raw.TotalCount, Workflows: make([]workflowJSON, 0, len(*raw.Workflows))}
	for _, item := range *raw.Workflows {
		trimmedItem, ok := item.trim()
		if !ok {
			return "", false
		}
		out.Workflows = append(out.Workflows, trimmedItem)
	}
	rendered, err := marshalTrimmed(out)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
