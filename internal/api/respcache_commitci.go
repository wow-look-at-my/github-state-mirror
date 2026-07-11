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
// contract, like respcache.go):
//
//	GET /repos/{owner}/{repo}/commits/{ref}/status      (combined commit status)
//	GET /repos/{owner}/{repo}/commits/{ref}/check-runs  (check runs for a ref)
//	GET /repos/{owner}/{repo}/commits/{ref}/statuses    (raw statuses LIST)
//	GET /repos/{owner}/{repo}/statuses/{ref}            (its legacy alias)
//
// Fleet-wide CI watchers poll these endpoints per repo/branch/sha -- hundreds
// of passthroughs per sweep in the request log -- and between CI events the
// answers are stable. The required-builds hook paginates check-runs AND the
// raw statuses list at per_page=100&page=N until a short page (consumer
// survey 2026-07-11), so pagination is part of the cache key: per_page/page
// are parsed like the PR-files route and a param-less request stores under
// GitHub's defaults. The ref is treated as an OPAQUE key: a branch name
// (slashes and all), a sha, or a tag is cached verbatim, never resolved, so
// each spelling is its own snapshot. The /commits/* subtree's OTHER tails
// (the single-commit read /commits/{sha}, /check-suites, /pulls, /comments,
// ...) are not modeled and are forwarded to the passthrough proxy unchanged.
//
// The raw statuses list has TWO path spellings for one resource: the legacy
// /repos/{owner}/{repo}/statuses/{ref} alias (what required-builds actually
// sends) and the modern /commits/{ref}/statuses form. GitHub answers both
// identically, so both registrations land in ONE handler and ONE row space
// (kind = statuses_list, ref verbatim) -- a read through either spelling
// warms the other.
//
// These routes deliberately do NOT read or write the commit_checks truth
// table: its normalized per-context rows are lossy against these responses
// (no timestamps, no descriptions, no run ids). Unifying the two is possible
// future work; the whole trimmed document is snapshotted per exact request.
//
// Invalidation is the load-bearing part: status/check_run/check_suite events
// flush the payload-named refs' rows (every kind, every page; repo-wide when
// the payload names none), push flushes the pushed ref, and repository
// flushes like every response cache. Net effect: snapshots only survive
// while a repo's CI is quiet -- exactly when the fleet sweeps re-poll them.
// A 24h TTL backstops missed deliveries.

// commitCICacheTTL bounds how long a MISSED CI/push delivery could leave a
// stale snapshot being served. Webhooks flush sooner; this is the backstop.
const commitCICacheTTL = 24 * time.Hour

const (
	// commitCIDefaultPerPage is GitHub's default page size on all three
	// listing forms when the request does not send per_page.
	commitCIDefaultPerPage = 30

	// commitCIMaxCachedPage caps which pages are modeled. The CI consumers
	// page shallowly (a commit with >10 pages of statuses at per_page=100 is
	// pathological); deeper pagination passes through.
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
	h.ghProxy.ServeHTTP(w, r)
}

// statusesAlias serves GET /repos/{owner}/{repo}/statuses/{ref} -- the LEGACY
// spelling of the raw statuses list, and the one the consumers actually send
// (required-builds' listStatuses; survey 2026-07-11). The wildcard is the
// whole ref (slashes and all); it lands in the same handler and the same
// (kind = statuses_list) row space as the modern /commits/{ref}/statuses
// form, since GitHub answers both identically. Only GET is registered, so
// the required-builds status PUBLISH -- POST /repos/{o}/{r}/statuses/{sha} --
// falls to MethodNotAllowed and the passthrough proxy, untouched.
func (h *handlers) statusesAlias(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "*")
	if ref == "" {
		h.ghProxy.ServeHTTP(w, r)
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

	// Only the default JSON representation with the modeled paging shape is
	// cached: the check-runs filters (?check_name, ?status, ?filter, ?app_id)
	// change the body's contents entirely and pass through, as does anything
	// else parseCommitCIShape rejects.
	if !acceptsDefaultJSON(r) {
		h.ghProxy.ServeHTTP(w, r)
		return
	}
	perPage, page, ok := parseCommitCIShape(r.URL.Query())
	if !ok {
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
	if c, ok, err := h.store.GetCachedCommitCI(r.Context(), owner, repo, ref, kind, perPage, page, now); err != nil {
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
	}, perPage, page, now, commitCICacheTTL); err != nil {
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

// statusListItemJSON is one trimmed entry of the raw statuses LIST. The
// consumers' contract (required-builds' listStatuses, survey 2026-07-11):
// context/state/description/target_url are read, deduplication is by context
// FIRST-WINS relying on the response's newest-first order -- so the rebuild
// preserves item order EXACTLY, and description/target_url are nullable
// strings whose keys are ALWAYS emitted (null when null, matching GitHub).
// target_url is a pinned exception to the no-URL doctrine (the hook renders
// it as the build's details link); the per-status id/node_id, the creator
// user object, and url/avatar_url stay dropped.
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

// appIDJSON is a check run's producing app, trimmed to its id -- the one app
// field the org's known consumer contract (required-builds-manager's
// own-check filter) branches on. The rest of GitHub's app object (slug, name,
// owner, permissions, events, urls) is dropped.
type appIDJSON struct {
	ID int64 `json:"id"`
}

// checkRunOutputJSON is a check run's output, trimmed to its title -- the one
// output field the consumers read (required-builds renders output.title on
// its breakdown page; survey 2026-07-11). summary/text stay dropped: they are
// UNBOUNDED display markdown no mirror-pointed consumer reads.
type checkRunOutputJSON struct {
	Title *string `json:"title"` // nullable; key always emitted
}

// checkRunItemJSON is one trimmed entry of the check_runs array: the bounded
// state fields a CI watcher branches on. conclusion/started_at/completed_at
// are nullable while a run is queued/in progress and the keys are always
// emitted, exactly as upstream. The 2026-07-11 consumer survey re-added three
// fields the 2026-07-05 survey had dropped -- required-builds reads
// output.title (trimmed to exactly that; see checkRunOutputJSON) and renders
// details_url/html_url as its breakdown links, so both URL fields are PINNED
// exceptions to the no-URL doctrine (nullable pointers, keys always emitted:
// GitHub sends details_url null for runs without one). Still dropped: `url`,
// node_id/external_id, check_suite, pull_requests, and the rest of `output`
// (summary/text -- unbounded display markdown).
type checkRunItemJSON struct {
	ID          int64              `json:"id"`
	HeadSHA     string             `json:"head_sha"`
	Name        string             `json:"name"`
	Status      string             `json:"status"`
	Conclusion  *string            `json:"conclusion"`   // nullable until completed
	StartedAt   *string            `json:"started_at"`   // nullable while queued
	CompletedAt *string            `json:"completed_at"` // nullable until completed
	App         *appIDJSON         `json:"app"`          // nullable; trimmed to {id}
	Output      checkRunOutputJSON `json:"output"`       // always emitted; trimmed to {title}
	DetailsURL  *string            `json:"details_url"`  // nullable; pinned consumer-read exception
	HTMLURL     *string            `json:"html_url"`     // nullable; pinned consumer-read exception
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
			Output *struct {
				Title *string `json:"title"`
			} `json:"output"`
			DetailsURL *string `json:"details_url"`
			HTMLURL    *string `json:"html_url"`
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
			DetailsURL: cr.DetailsURL, HTMLURL: cr.HTMLURL,
		}
		if cr.App != nil {
			item.App = &appIDJSON{ID: cr.App.ID}
		}
		// GitHub always sends the output object on real check runs; a missing
		// or null one still rebuilds as {"title": null} so the key is stable.
		if cr.Output != nil {
			item.Output = checkRunOutputJSON{Title: cr.Output.Title}
		}
		doc.CheckRuns = append(doc.CheckRuns, item)
	}
	rendered, err := marshalTrimmed(doc)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
