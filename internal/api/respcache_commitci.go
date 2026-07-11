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

// This file implements the cached commit-CI routes (tier 2 of the cache
// contract, like respcache.go):
//
//	GET /repos/{owner}/{repo}/commits/{ref}/status      (combined commit status)
//	GET /repos/{owner}/{repo}/commits/{ref}/check-runs  (check runs for a ref)
//
// Fleet-wide CI watchers poll both endpoints per repo/branch/sha -- hundreds
// of passthroughs per sweep in the request log -- and between CI events the
// answers are stable. The ref is treated as an OPAQUE key: a branch name
// (slashes and all), a sha, or a tag is cached verbatim, never resolved, so
// each spelling is its own snapshot. Both routes live under the /commits/*
// subtree, whose OTHER tails (the single-commit read /commits/{sha}, the raw
// /statuses list, /check-suites, /pulls, /comments, ...) are not modeled and
// are forwarded to the passthrough proxy unchanged.
//
// These routes deliberately do NOT read or write the commit_checks truth
// table: its normalized per-context rows are lossy against these responses
// (no timestamps, no descriptions, no run ids). Unifying the two is possible
// future work; for v1 the whole trimmed document is snapshotted per ref.
//
// Invalidation is the load-bearing part: status/check_run/check_suite events
// flush the repo's rows (CI state changed somewhere in the repo -- per-sha
// precision is not worth the bookkeeping for v1), push flushes too (a
// branch-form ref's tip moved; a brand-new sha has no rows yet anyway), and
// repository flushes like every response cache. Net effect: snapshots only
// survive while a repo's CI is quiet -- exactly when the fleet sweeps re-poll
// them. A 24h TTL backstops missed deliveries.

// commitCICacheTTL bounds how long a MISSED CI/push delivery could leave a
// stale snapshot being served. Webhooks flush sooner; this is the backstop.
const commitCICacheTTL = 24 * time.Hour

// stage-2: the bare param-less requests this route models today store and
// read under GitHub's default pagination shape; stage 2 parses real
// ?per_page/?page values (and adds the statuses-list kind) instead of these
// constants.
const (
	commitCIDefaultPerPage = 30
	commitCIDefaultPage    = 1
)

// commitsSubtree dispatches GET /repos/{owner}/{repo}/commits/* by its path
// tail: a `{ref}/status` tail is the cached combined-status route and a
// `{ref}/check-runs` tail the cached check-runs route -- the suffix anchor is
// what lets a ref carry slashes (claude/my-branch/status), which a
// single-segment route parameter could never match. Every other tail (the
// single-commit read, /statuses, /check-suites, ...) is forwarded to the
// passthrough proxy, exactly as it was before this subtree was registered.
func (h *handlers) commitsSubtree(w http.ResponseWriter, r *http.Request) {
	tail := chi.URLParam(r, "*")
	if ref, ok := strings.CutSuffix(tail, "/status"); ok && ref != "" {
		h.cachedCommitCI(w, r, ref, ghdata.CommitCIKindStatus, denyKindCommitStatus)
		return
	}
	if ref, ok := strings.CutSuffix(tail, "/check-runs"); ok && ref != "" {
		h.cachedCommitCI(w, r, ref, ghdata.CommitCIKindCheckRuns, denyKindCheckRuns)
		return
	}
	h.ghProxy.ServeHTTP(w, r)
}

// cachedCommitCI serves one commit-CI snapshot (combined status or check
// runs) from absorbed state, fetching and absorbing on a miss.
func (h *handlers) cachedCommitCI(w http.ResponseWriter, r *http.Request, ref, kind, denyKind string) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))

	// Only the default JSON representation with NO query params is modeled:
	// ?per_page/?page change which entries the body carries, and the
	// check-runs filters (?check_name, ?status, ?filter, ?app_id) change its
	// contents entirely. A caller sending any of them passes through.
	if !acceptsDefaultJSON(r) || r.URL.RawQuery != "" {
		h.ghProxy.ServeHTTP(w, r)
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKind, owner+"/"+repo+"/commits/"+ref); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if c, ok, err := h.store.GetCachedCommitCI(r.Context(), owner, repo, ref, kind, commitCIDefaultPerPage, commitCIDefaultPage, now); err != nil {
		slog.Warn("commit CI cache read failed", "owner", owner, "repo", repo, "ref", ref, "kind", kind, "error", err)
	} else if ok {
		h.serveCommitCI(w, r, c.Doc, true)
		return
	}

	// Miss: fetch from GitHub with the caller's own credentials.
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	doc, absorbed := absorbCommitCI(kind, resp.StatusCode, body)
	if overflow || !absorbed {
		// Includes 404 (unknown ref -- it can be pushed later) and 5xx:
		// relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedCommitCI(r.Context(), ghdata.CachedCommitCI{
		Owner: owner, Repo: repo, Ref: ref, Kind: kind, Doc: doc,
	}, commitCIDefaultPerPage, commitCIDefaultPage, now, commitCICacheTTL); err != nil {
		slog.Warn("commit CI cache write failed", "owner", owner, "repo", repo, "ref", ref, "kind", kind, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispMiss, resp.StatusCode)
	h.serveCommitCI(w, r, doc, false)
}

// serveCommitCI writes the trimmed document. The doc is rendered once at
// absorb time and stored verbatim, so hit and miss serve identical bytes.
func (h *handlers) serveCommitCI(w http.ResponseWriter, r *http.Request, doc string, hit bool) {
	if hit {
		h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, DispHit)
	}
	writeRebuilt(w, http.StatusOK, []byte(doc), hit)
}

// absorbCommitCI parses a commit-CI 200 into the trimmed document (rendered
// once here; hits serve the stored bytes). Reports false -- serve verbatim,
// store nothing -- for any other status or any shape the model cannot hold.
func absorbCommitCI(kind string, status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false
	}
	if kind == ghdata.CommitCIKindCheckRuns {
		return absorbCheckRuns(trimmed)
	}
	return absorbCombinedStatus(trimmed)
}

// commitStatusItemJSON is one trimmed entry of the combined status's statuses
// array: the state fields only. The per-status id/node_id, avatar_url/url,
// and target_url are dropped -- no mirror-pointed consumer reads them (Step-0
// survey, 2026-07-05); target_url is the one a future dashboard might want
// back, and re-adding it is a one-line change here plus a pin in the no-URL
// test.
type commitStatusItemJSON struct {
	Context     string  `json:"context"`
	State       string  `json:"state"`
	Description *string `json:"description"` // nullable; GitHub always sends the key
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// combinedStatusJSON is the trimmed rebuild of a combined commit status:
// {state, sha, total_count, statuses:[...]}. The full repository object and
// the url/commit_url fields are dropped. sha is the RESOLVED tip the answer
// described -- exactly why a push must flush branch-form rows.
type combinedStatusJSON struct {
	State      string                 `json:"state"`
	SHA        string                 `json:"sha"`
	TotalCount int64                  `json:"total_count"`
	Statuses   []commitStatusItemJSON `json:"statuses"`
}

// absorbCombinedStatus parses a combined-status 200 into the trimmed
// document. The statuses array must be PRESENT (it is always present
// upstream, empty when the ref has no statuses -- state reads "pending"
// there) and the resolved sha must be a full hex object id.
func absorbCombinedStatus(trimmed []byte) (string, bool) {
	var raw struct {
		State    string `json:"state"`
		SHA      string `json:"sha"`
		Statuses *[]struct {
			Context     string  `json:"context"`
			State       string  `json:"state"`
			Description *string `json:"description"`
			CreatedAt   string  `json:"created_at"`
			UpdatedAt   string  `json:"updated_at"`
		} `json:"statuses"`
		TotalCount int64 `json:"total_count"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.State == "" || raw.Statuses == nil {
		return "", false
	}
	sha := strings.ToLower(raw.SHA)
	if !isFullHexSHA(sha) {
		return "", false
	}
	doc := combinedStatusJSON{
		State: raw.State, SHA: sha, TotalCount: raw.TotalCount,
		Statuses: make([]commitStatusItemJSON, 0, len(*raw.Statuses)),
	}
	for _, s := range *raw.Statuses {
		if s.Context == "" || s.State == "" {
			return "", false
		}
		doc.Statuses = append(doc.Statuses, commitStatusItemJSON{
			Context: s.Context, State: s.State, Description: s.Description,
			CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
		})
	}
	rendered, err := marshalTrimmed(doc)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}

// appIDJSON is a check run's producing app, trimmed to its id -- the one app
// field the org's known consumer contract (required-builds-manager's
// own-check filter) branches on. The rest of GitHub's app object (slug, name,
// owner, permissions, events, urls) is dropped.
type appIDJSON struct {
	ID int64 `json:"id"`
}

// checkRunItemJSON is one trimmed entry of the check_runs array: the bounded
// state fields a CI watcher branches on. conclusion/started_at/completed_at
// are nullable while a run is queued/in progress and the keys are always
// emitted, exactly as upstream. Dropped: html_url/details_url/url (no
// mirror-pointed consumer reads them -- Step-0 survey, 2026-07-05),
// node_id/external_id, check_suite, pull_requests, and the UNBOUNDED `output`
// object (title/summary/text can be hundreds of KB of display markdown; no
// consumer reads it through the mirror).
type checkRunItemJSON struct {
	ID          int64      `json:"id"`
	HeadSHA     string     `json:"head_sha"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  *string    `json:"conclusion"`   // nullable until completed
	StartedAt   *string    `json:"started_at"`   // nullable while queued
	CompletedAt *string    `json:"completed_at"` // nullable until completed
	App         *appIDJSON `json:"app"`          // nullable; trimmed to {id}
}

// checkRunsJSON is the trimmed rebuild of a check-runs listing:
// {total_count, check_runs:[...]}. total_count is GitHub's TOTAL (it can
// exceed the page the bare query returned -- the snapshot is that exact
// query's answer, like the commits list).
type checkRunsJSON struct {
	TotalCount int64              `json:"total_count"`
	CheckRuns  []checkRunItemJSON `json:"check_runs"`
}

// absorbCheckRuns parses a check-runs 200 into the trimmed document. The
// check_runs array must be PRESENT (always present upstream, empty when the
// ref has none) and every run must carry a status and a full-hex head sha.
func absorbCheckRuns(trimmed []byte) (string, bool) {
	var raw struct {
		TotalCount int64 `json:"total_count"`
		CheckRuns  *[]struct {
			ID          int64   `json:"id"`
			HeadSHA     string  `json:"head_sha"`
			Name        string  `json:"name"`
			Status      string  `json:"status"`
			Conclusion  *string `json:"conclusion"`
			StartedAt   *string `json:"started_at"`
			CompletedAt *string `json:"completed_at"`
			App         *struct {
				ID int64 `json:"id"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.CheckRuns == nil {
		return "", false
	}
	doc := checkRunsJSON{
		TotalCount: raw.TotalCount,
		CheckRuns:  make([]checkRunItemJSON, 0, len(*raw.CheckRuns)),
	}
	for _, cr := range *raw.CheckRuns {
		sha := strings.ToLower(cr.HeadSHA)
		if cr.Status == "" || !isFullHexSHA(sha) {
			return "", false
		}
		item := checkRunItemJSON{
			ID: cr.ID, HeadSHA: sha, Name: cr.Name, Status: cr.Status,
			Conclusion: cr.Conclusion, StartedAt: cr.StartedAt, CompletedAt: cr.CompletedAt,
		}
		if cr.App != nil {
			item.App = &appIDJSON{ID: cr.App.ID}
		}
		doc.CheckRuns = append(doc.CheckRuns, item)
	}
	rendered, err := marshalTrimmed(doc)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
