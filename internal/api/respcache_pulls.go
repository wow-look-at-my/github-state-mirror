package api

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// Cached PR REST routes (tier 2 of the cache contract, like respcache.go): the open-PR list, the single PR,
// and the repo installation lookup. see docs/cache/rest-routes.md

const (
	// pullsListCacheTTL is only the backstop for a missed pull_request delivery; webhooks are the maintenance.
	pullsListCacheTTL = 24 * time.Hour

	// closedPullCacheTTL is the same accepted staleness class as PRRowFresh, for a missed reopen/edit/relabel.
	closedPullCacheTTL = 24 * time.Hour

	// pullsDefaultPerPage is GitHub's default page size when per_page is absent.
	pullsDefaultPerPage = 30
)

// ---- GET /repos/{owner}/{repo}/pulls ----

// pullsListShape is a parsed, cacheable /pulls query: the shapes pr-minder
type pullsListShape struct {
	perPage int
	head    string // "" = unfiltered; else "owner:branch"
	base    string // "" = unfiltered; else a base branch name
}

// parsePullsListShape reports the shape of a /pulls query and whether the
// cache models it. Unknown params, repeated params, state != open, a
// malformed head, an out-of-range per_page, or any page other than the first
// make it non-cacheable (page > 1 only ever follows a full page 1, which is
// itself never served from state).
//
// head and base are both pure FILTERS over the open set the completeness
// marker already vouches for -- given the whole open set, selecting the PRs
// with a given head or base is exactly what GitHub does -- so they are
// answered from state and, like any filtered response, never used to SET the
// marker.
func parsePullsListShape(q url.Values) (pullsListShape, bool) {
	shape := pullsListShape{perPage: pullsDefaultPerPage}
	for key, vals := range q {
		if len(vals) != 1 {
			return shape, false
		}
		v := vals[0]
		switch key {
		case "state":
			if v != "open" {
				return shape, false
			}
		case "per_page":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return shape, false
			}
			shape.perPage = n
		case "page":
			if n, err := strconv.Atoi(v); err != nil || n != 1 {
				return shape, false
			}
		case "head":
			owner, branch, ok := strings.Cut(v, ":")
			if !ok || owner == "" || branch == "" {
				return shape, false
			}
			shape.head = v
		case "base":
			if v == "" {
				return shape, false
			}
			shape.base = v
		default:
			return shape, false
		}
	}
	return shape, true
}

// filterPullRows applies the head=owner:branch filter the way GitHub does:
// branch name exact, head-repo owner case-insensitive. A row with no head
// repo (deleted fork) can never match an owner-qualified filter.
func filterPullRows(rows []dbgen.PullRequest, head, base string) []dbgen.PullRequest {
	if head == "" && base == "" {
		return rows
	}
	hOwner, hBranch, _ := strings.Cut(head, ":")
	var out []dbgen.PullRequest
	for _, pr := range rows {
		if head != "" {
			if !pr.HeadRefName.Valid || pr.HeadRefName.String != hBranch {
				continue
			}
			if !pr.HeadRepoFullName.Valid {
				continue
			}
			repoOwner, _, ok := strings.Cut(pr.HeadRepoFullName.String, "/")
			if !ok || !strings.EqualFold(repoOwner, hOwner) {
				continue
			}
		}
		if base != "" {
			// The base branch always lives in THIS repo, so the bare name is
			// the whole filter (unlike head, which can name a fork).
			if !pr.BaseRefName.Valid || pr.BaseRefName.String != base {
				continue
			}
		}
		out = append(out, pr)
	}
	return out
}

// cachedPullsList serves a repo's open-PR list from webhook-maintained state
// once a complete list has been absorbed, fetching and absorbing on a miss.
func (h *handlers) cachedPullsList(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")

	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	shape, ok := parsePullsListShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindRepoPulls, ghdata.NormalizeRepoKey(owner)+"/"+ghdata.NormalizeRepoKey(repo)+"/pulls"); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if fresh, err := h.store.PullsListFresh(r.Context(), owner, repo, now); err != nil {
		slog.Warn("pulls list marker read failed", "owner", owner, "repo", repo, "error", err)
	} else if fresh {
		rows, labelsByPR, err := h.store.RestPullsList(r.Context(), owner, repo)
		if err != nil {
			slog.Warn("pulls list cache read failed", "owner", owner, "repo", repo, "error", err)
		} else if allRestComplete(rows) {
			filtered := filterPullRows(rows, shape.head, shape.base)
			// Only a provably-single-page answer is served from state; a full page may continue upstream.
			if len(filtered) < shape.perPage {
				h.servePullsList(w, r, filtered, labelsByPR, true)
				return
			}
		}
	}

	// Miss: fetch from GitHub with the caller's own credentials.
	fetchStart := time.Now()
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	rows, labelsByPR, absorbed := absorbPullsListBody(owner, repo, resp.StatusCode, body)
	if overflow || !absorbed {
		h.replayUnstored(w, r, resp, body)
		return
	}
	// Only an unfiltered, not-full page proves the complete open set; a filtered or full page still absorbs rows.
	complete := shape.head == "" && shape.base == "" && len(rows) < shape.perPage
	absorbOwner, absorbRepo := owner, repo
	if len(rows) > 0 {
		absorbOwner, absorbRepo = rows[0].Owner, rows[0].Repo
	}
	if err := h.store.AbsorbPullsList(r.Context(), absorbOwner, absorbRepo, rows, labelsByPR, complete, fetchStart, now, pullsListCacheTTL); err != nil {
		slog.Warn("pulls list absorb failed", "owner", owner, "repo", repo, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	h.servePullsList(w, r, rows, labelsByPR, false)
}

// servePullsList rebuilds and writes the trimmed list. Hit and miss serve the
// same shape; a miss keeps GitHub's own response order, a hit serves
// newest-created first (GitHub's default sort).
func (h *handlers) servePullsList(w http.ResponseWriter, r *http.Request, rows []dbgen.PullRequest, labelsByPR map[int64][]dbgen.PrLabel, hit bool) {
	items := make([]pullListItemJSON, 0, len(rows))
	now := time.Now()
	for _, pr := range rows {
		// Belt and braces, like the single-PR hit gate: a row must never serve a push-invalidated test-merge sha.
		if ghdata.PRMergeShaStale(pr, now) {
			pr.MergeCommitSha = sql.NullString{}
		}
		items = append(items, renderPullListItem(pr, labelsByPR[pr.Number]))
	}
	body, err := marshalTrimmed(items)
	if err != nil {
		slog.Warn("pulls list render failed", "path", r.URL.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hit {
		h.reqlog.observe(r, DispHit)
	}
	writeRebuilt(w, http.StatusOK, body, hit)
}

// allRestComplete reports whether every row can be rebuilt as a REST response (GraphQL-sourced rows cannot).
func allRestComplete(rows []dbgen.PullRequest) bool {
	for _, pr := range rows {
		if !ghdata.PRRestComplete(pr) {
			return false
		}
	}
	return true
}

// ---- GET /repos/{owner}/{repo}/pulls/{number} ----

// cachedPull serves a single PR from state: an OPEN row when it is
// rest-complete AND its mergeable is known, or a CLOSED PR's rendered doc
// (closed_pull_cache -- every drain re-reads settled PRs, and each read used
// to be a fresh passthrough). Everything else -- unknown or null mergeable,
// unknown PRs, non-numeric path segments like /pulls/comments, query params,
// non-default Accept -- misses or passes through. A fetched open PR is
// absorbed (including GitHub's computed mergeable, null and all) and drops
// any stale closed doc; a fetched closed PR deletes any cached open row (the
// truth table retains open PRs only) and is absorbed as a whole-doc snapshot,
// served rebuilt.
func (h *handlers) cachedPull(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	numStr := chi.URLParam(r, "number")

	number, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil || number <= 0 || r.URL.RawQuery != "" {
		h.passthrough(w, r, shapeReason(r, err == nil && number > 0))
		return
	}
	if !acceptsDefaultJSON(r) {
		// The DIFF representation gets the 406-verdict flow (see the file
		// comment); every other non-default Accept keeps today's passthrough.
		if acceptsPullDiff(r) {
			h.cachedPullDiff(w, r, owner, repo, number, numStr)
			return
		}
		h.passthrough(w, r, PassAccept)
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindPull, ghdata.NormalizeRepoKey(owner)+"/"+ghdata.NormalizeRepoKey(repo)+"#"+numStr); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	// The hit gate: rest-complete, mergeable KNOWN, the stored test-merge sha
	// not the push-invalidated one (belt and braces -- the guarded writes null
	// that sha instead of storing it), and recently touched.
	if pr, labels, ok, err := h.store.RestSinglePull(r.Context(), owner, repo, number); err != nil {
		slog.Warn("single PR cache read failed", "owner", owner, "repo", repo, "number", number, "error", err)
	} else if ok && ghdata.PRRestComplete(pr) && mergeableKnown(pr) && diffStatsKnown(pr) &&
		!ghdata.PRMergeShaStale(pr, time.Now()) && ghdata.PRRowFresh(pr, time.Now()) {
		h.serveSinglePull(w, r, pr, labels, true)
		return
	}

	// No servable open row -- a CLOSED PR's rendered doc answers instead.
	if doc, ok, err := h.store.GetCachedClosedPull(r.Context(), owner, repo, number, time.Now()); err != nil {
		slog.Warn("closed PR cache read failed", "owner", owner, "repo", repo, "number", number, "error", err)
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

	if overflow || resp.StatusCode != http.StatusOK {
		h.replayUnstored(w, r, resp, body)
		return
	}
	var raw restPRJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		h.replayUnstored(w, r, resp, body)
		return
	}
	pr, labels, ok := absorbRestPR(owner, repo, raw)
	if !ok {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if pr.State != "OPEN" {
		// Closed/merged: the truth table retains open PRs only, so drop any
		// stale row -- and absorb GitHub's answer as a rendered whole-doc
		// snapshot, served rebuilt (hit and miss byte-identical).
		if err := h.store.DeletePR(r.Context(), pr.Owner, pr.Repo, pr.Number, pr.UpdatedAt, time.Now()); err != nil {
			slog.Warn("delete closed PR row failed", "owner", pr.Owner, "repo", pr.Repo, "number", pr.Number, "error", err)
		}
		doc, mErr := marshalTrimmed(renderClosedPull(pr, labels, raw.Merged != nil && *raw.Merged))
		if mErr != nil {
			slog.Warn("closed PR render failed", "path", r.URL.Path, "error", mErr)
			h.replayUnstored(w, r, resp, body)
			return
		}
		if err := h.store.PutCachedClosedPull(r.Context(), owner, repo, number, string(doc), time.Now(), closedPullCacheTTL); err != nil {
			slog.Warn("closed PR doc store failed", "owner", owner, "repo", repo, "number", number, "error", err)
		}
		h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
		h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
		writeRebuilt(w, http.StatusOK, doc, false)
		return
	}
	staleRejected, aerr := h.store.AbsorbSinglePull(r.Context(), pr, labels, time.Now())
	if aerr != nil {
		slog.Warn("single PR absorb failed", "owner", pr.Owner, "repo", pr.Repo, "number", pr.Number, "error", aerr)
	}
	// A reopened PR must not keep serving its stale closed doc.
	if err := h.store.InvalidateClosedPullForPR(r.Context(), owner, repo, number); err != nil {
		slog.Warn("closed PR doc invalidate failed", "owner", owner, "repo", repo, "number", number, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	if staleRejected {
		pr.Mergeable = sql.NullString{}
		pr.MergeableState = sql.NullString{}
		pr.MergeCommitSha = sql.NullString{}
	}
	h.serveSinglePull(w, r, pr, labels, false)
}

func (h *handlers) serveSinglePull(w http.ResponseWriter, r *http.Request, pr dbgen.PullRequest, labels []dbgen.PrLabel, hit bool) {
	body, err := marshalTrimmed(renderSinglePull(pr, labels))
	if err != nil {
		slog.Warn("single PR render failed", "path", r.URL.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hit {
		h.reqlog.observe(r, DispHit)
	}
	writeRebuilt(w, http.StatusOK, body, hit)
}

// mergeableKnown gates both mergeable and mergeable_state, so a hit never serves a state GitHub has since left.
// see docs/cache/rest-routes.md
func mergeableKnown(pr dbgen.PullRequest) bool {
	return pr.Mergeable.Valid &&
		(pr.Mergeable.String == "MERGEABLE" || pr.Mergeable.String == "CONFLICTING") &&
		pr.MergeableState.Valid && pr.MergeableState.String != "" && pr.MergeableState.String != "unknown"
}

// diffStatsKnown gates on additions/deletions, which only a single-PR answer supplies.
// see docs/cache/rest-routes.md
func diffStatsKnown(pr dbgen.PullRequest) bool {
	return pr.Additions.Valid && pr.Deletions.Valid
}

// ---- small helpers ----

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func nullableStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// normalizeRESTTime folds a REST timestamp to the fixed-width UTC RFC3339 form the schema stores.
func normalizeRESTTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339)
}
