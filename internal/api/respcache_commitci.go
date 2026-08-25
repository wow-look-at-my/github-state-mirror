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

// This file implements the cached commit-CI routes (tier 2 of the cache
// contract): combined status, check-runs, and the raw statuses list.
// see docs/cache/rest-routes.md

// commitCICacheTTL bounds a missed CI/push delivery's staleness; it lives here because a `status` delivery re-dates a rewritten document exactly like a fetch would.
const commitCICacheTTL = ghdata.CommitCICacheTTL

const (
	// commitCIDefaultPerPage is GitHub's default page size across all three listing forms.
	commitCIDefaultPerPage = 30

	// commitCIMaxCachedPage caps modeled pages; deeper pagination (pathological in practice) passes through.
	commitCIMaxCachedPage = 10
)

// parseCommitCIShape reports the paging shape of a commit-CI query and
// whether the cache models it (modeled on parsePullFilesShape). Unknown
// params (the check-runs filters ?check_name/?status/?filter/?app_id change
// the body's contents entirely), repeated params, an out-of-range per_page,
// or a page beyond the modeled cap make it non-cacheable.
func parseCommitCIShape(q url.Values) (perPage, page int, ok bool) {
	perPage, page = commitCIDefaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return 0, 0, false
		}
		v := vals[0]
		switch key {
		case "per_page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return 0, 0, false
			}
			perPage = n
		case "page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > commitCIMaxCachedPage {
				return 0, 0, false
			}
			page = n
		default:
			return 0, 0, false
		}
	}
	return perPage, page, true
}

// commitsSubtree dispatches GET /repos/{owner}/{repo}/commits/* by its path
// tail: a `{ref}/status` tail is the cached combined-status route, a
// `{ref}/check-runs` tail the cached check-runs route, and a `{ref}/statuses`
// tail the cached raw statuses list -- the suffix anchor is what lets a ref
// carry slashes (claude/my-branch/status), which a single-segment route
// parameter could never match. Every other tail (the single-commit read,
// /check-suites, ...) is forwarded to the passthrough proxy, exactly as it
// was before this subtree was registered.
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
	if ref, ok := strings.CutSuffix(tail, "/statuses"); ok && ref != "" {
		h.cachedCommitCI(w, r, ref, ghdata.CommitCIKindStatusesList, denyKindStatusesList)
		return
	}
	h.passthrough(w, r, PassPath)
}

// statusesAlias serves the legacy /statuses/{ref} spelling of the raw
// statuses list; POST falls to MethodNotAllowed and the passthrough proxy.
// see docs/cache/rest-routes.md
func (h *handlers) statusesAlias(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "*")
	if ref == "" {
		h.passthrough(w, r, PassPath)
		return
	}
	h.cachedCommitCI(w, r, ref, ghdata.CommitCIKindStatusesList, denyKindStatusesList)
}

// cachedCommitCI serves one commit-CI snapshot (combined status, check runs,
// or the raw statuses list) from absorbed state, fetching and absorbing on a
// miss. All three kinds share the pagination shape parse: per_page/page join
// the cache key, so each paginated form is its own self-contained snapshot.
func (h *handlers) cachedCommitCI(w http.ResponseWriter, r *http.Request, ref, kind, denyKind string) {
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))

	// Only the default JSON representation with the modeled paging shape is cached; check-runs filters and anything else unmodeled pass through.
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	perPage, page, ok := parseCommitCIShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
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
	if c, ok, err := h.store.GetCachedCommitCI(r.Context(), owner, repo, ref, kind, perPage, page, now); err != nil {
		slog.Warn("commit CI cache read failed", "owner", owner, "repo", repo, "ref", ref, "kind", kind, "error", err)
	} else if ok {
		// The stored row's status is what was absorbed: 200 (a real snapshot) or 404 (an unknown-ref verdict).
		h.serveCommitCI(w, r, c.Status, c.Doc, true)
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
	doc, absorbed := absorbCommitCI(kind, resp.StatusCode, body)
	if !absorbed && !overflow && resp.StatusCode == http.StatusNotFound {
		// The 404 unknown-ref VERDICT is absorbed too, honest via the same flushes as a 200 row.
		// see docs/cache/rest-routes.md
		if doc404, mErr := marshalTrimmed(notFoundJSON{Message: upstreamErrorMessage(body), Status: "404"}); mErr == nil {
			doc, absorbed, status = string(doc404), true, http.StatusNotFound
		}
	}
	if overflow || !absorbed {
		// 403 and 5xx: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedCommitCI(r.Context(), ghdata.CachedCommitCI{
		Owner: owner, Repo: repo, Ref: ref, Kind: kind, Status: status, Doc: doc,
	}, perPage, page, now, commitCICacheTTL); err != nil {
		slog.Warn("commit CI cache write failed", "owner", owner, "repo", repo, "ref", ref, "kind", kind, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	h.serveCommitCI(w, r, status, doc, false)
}

// serveCommitCI writes the stored commit-CI document under the status it
// absorbed (200 snapshot / 404 verdict). The doc is rendered once at absorb
// time and stored verbatim, so hit and miss serve identical bytes.
func (h *handlers) serveCommitCI(w http.ResponseWriter, r *http.Request, status int, doc string, hit bool) {
	if hit {
		if status == http.StatusOK {
			h.reqlog.observe(r, DispHit)
		} else {
			h.reqlog.observeStatus(r, DispHit, status)
		}
	}
	writeRebuilt(w, status, []byte(doc), hit)
}

// absorbCommitCI parses a commit-CI 200 into the trimmed document (rendered
// once here; hits serve the stored bytes). Reports false -- serve verbatim,
// store nothing -- for any other status or any shape the model cannot hold.
// The statuses LIST is a bare JSON array; the other two kinds are objects.
func absorbCommitCI(kind string, status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "", false
	}
	switch kind {
	case ghdata.CommitCIKindCheckRuns:
		if trimmed[0] != '{' {
			return "", false
		}
		return absorbCheckRuns(trimmed)
	case ghdata.CommitCIKindStatusesList:
		if trimmed[0] != '[' {
			return "", false
		}
		return absorbStatusesList(trimmed)
	default:
		if trimmed[0] != '{' {
			return "", false
		}
		return absorbCombinedStatus(trimmed)
	}
}

// commitStatusItemJSON is one trimmed entry of the combined status's statuses array: state fields only.
// see docs/cache/rest-routes.md
type commitStatusItemJSON struct {
	Context     string  `json:"context"`
	State       string  `json:"state"`
	Description *string `json:"description"` // nullable; GitHub always sends the key
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// combinedStatusJSON is the trimmed rebuild of a combined commit status; sha is the RESOLVED tip, which is why a push must flush branch-form rows.
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

// statusListItemJSON is one trimmed entry of the raw statuses LIST; item order must be preserved exactly (consumers dedupe by context FIRST-WINS).
// see docs/cache/rest-routes.md
type statusListItemJSON struct {
	Context     string  `json:"context"`
	State       string  `json:"state"`
	Description *string `json:"description"` // nullable; key always emitted
	TargetURL   *string `json:"target_url"`  // nullable; pinned consumer-read exception
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// absorbStatusesList parses a raw statuses-list 200 -- a BARE JSON ARRAY,
// unlike the other two kinds -- into the trimmed document. Every item must
// carry a context and a state; an empty array (a ref with no statuses, or a
// page past the end) is a valid, cacheable answer.
func absorbStatusesList(trimmed []byte) (string, bool) {
	var raw []struct {
		Context     string  `json:"context"`
		State       string  `json:"state"`
		Description *string `json:"description"`
		TargetURL   *string `json:"target_url"`
		CreatedAt   string  `json:"created_at"`
		UpdatedAt   string  `json:"updated_at"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return "", false
	}
	items := make([]statusListItemJSON, 0, len(raw))
	for _, s := range raw {
		if s.Context == "" || s.State == "" {
			return "", false
		}
		items = append(items, statusListItemJSON{
			Context: s.Context, State: s.State,
			Description: s.Description, TargetURL: s.TargetURL,
			CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
		})
	}
	rendered, err := marshalTrimmed(items)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}

// The trimmed check-run shapes live in the store, so a check_run delivery's rewrite and this render always agree byte for byte.
type (
	checkRunItemJSON = ghdata.StoredCheckRun
	checkRunsJSON    = ghdata.StoredCheckRunsPage
)

// absorbCheckRuns parses a check-runs 200 into the trimmed document. The
// check_runs array must be PRESENT (always present upstream, empty when the
// ref has none) and every run must carry a status and a full-hex head sha.
func absorbCheckRuns(trimmed []byte) (string, bool) {
	var raw struct {
		TotalCount int64                 `json:"total_count"`
		CheckRuns  *[]ghdata.RawCheckRun `json:"check_runs"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.CheckRuns == nil {
		return "", false
	}
	doc := checkRunsJSON{
		TotalCount: raw.TotalCount,
		CheckRuns:  make([]checkRunItemJSON, 0, len(*raw.CheckRuns)),
	}
	for _, cr := range *raw.CheckRuns {
		item, ok := ghdata.TrimCheckRun(cr)
		if !ok {
			return "", false
		}
		doc.CheckRuns = append(doc.CheckRuns, item)
	}
	rendered, err := marshalTrimmed(doc)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
