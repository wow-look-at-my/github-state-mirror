package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// This file implements the cached PR REST routes (tier 2 of the cache
// contract, like respcache.go):
//
//   - GET /repos/{owner}/{repo}/pulls          (open-PR list)
//   - GET /repos/{owner}/{repo}/pulls/{number} (single PR)
//   - GET /repos/{owner}/{repo}/installation   (App-JWT authed, like the mint)
//
// The PR routes absorb into the SAME pull_requests/pr_labels tables the
// webhook dispatcher maintains, so pull_request webhooks keep served state
// current without invalidating anything. What gates serving:
//
//   LIST: a per-(actor, repo) "open-PR list complete" marker (pulls_list_cache)
//   proves the rows are the WHOLE open set. Absorbing a complete unfiltered
//   page sets it (24h TTL backstop); the GraphQL org-repos fetch clears it
//   (its replacement rows lack the REST-only columns); webhooks never touch it
//   (they ARE the maintenance). A rebuilt list as long as the request's
//   per_page may be truncated upstream -- served as a miss, never from state.
//
//   SINGLE: the row must be rest-complete AND its mergeable KNOWN. GitHub
//   computes `mergeable` lazily and pr-minder polls this endpoint waiting for
//   it to resolve, so an unknown/null mergeable always misses (fetch + absorb
//   the computed answer) -- the cache must never wedge that poll. Branch
//   pushes un-resolve the stored value (see NullPRMergeableForBranchForActors)
//   so a known answer can't go silently stale after either side moves.
//
// getPullDiff-style requests (Accept: application/vnd.github.diff etc.) pass
// through verbatim, exactly like the contents route's raw/html media types.

const (
	// pullsListCacheTTL bounds how long a MISSED pull_request delivery could
	// leave a stale absorbed list being served. Webhooks are the maintenance;
	// this is only the backstop.
	pullsListCacheTTL = 24 * time.Hour

	// repoInstallationCacheTTL is the TTL backstop on cached
	// GET /repos/{o}/{r}/installation answers; installation events flush
	// sooner.
	repoInstallationCacheTTL = 24 * time.Hour

	// pullsDefaultPerPage is GitHub's default page size for the list route
	// when the request does not send per_page.
	pullsDefaultPerPage = 30
)

// ---- GET /repos/{owner}/{repo}/pulls ----

// pullsListShape is a parsed, cacheable /pulls query: the shapes pr-minder
// sends (state=open + per_page/page, optionally head=owner:branch) plus the
// bare default. Anything else passes through.
type pullsListShape struct {
	perPage int
	head    string // "" = unfiltered; else "owner:branch"
}

// parsePullsListShape reports the shape of a /pulls query and whether the
// cache models it. Unknown params, repeated params, state != open, a
// malformed head, an out-of-range per_page, or any page other than the first
// make it non-cacheable (page > 1 only ever follows a full page 1, which is
// itself never served from state).
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
		default:
			return shape, false
		}
	}
	return shape, true
}

// filterPullRows applies the head=owner:branch filter the way GitHub does:
// branch name exact, head-repo owner case-insensitive. A row with no head
// repo (deleted fork) can never match an owner-qualified filter.
func filterPullRows(rows []dbgen.PullRequest, head string) []dbgen.PullRequest {
	if head == "" {
		return rows
	}
	hOwner, hBranch, _ := strings.Cut(head, ":")
	var out []dbgen.PullRequest
	for _, pr := range rows {
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
		h.ghProxy.ServeHTTP(w, r)
		return
	}
	shape, ok := parsePullsListShape(r.URL.Query())
	if !ok {
		h.ghProxy.ServeHTTP(w, r)
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
			filtered := filterPullRows(rows, shape.head)
			// Pagination guard: a rebuilt list as long as the requested page
			// could be truncated upstream -- only a provably-single-page
			// answer is served from state.
			if len(filtered) < shape.perPage {
				h.servePullsList(w, r, filtered, labelsByPR, true)
				return
			}
		}
	}

	// Miss: fetch from GitHub with the caller's own credentials.
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
	// The response proves the COMPLETE open set only when it is unfiltered
	// and not a full page (a full page may continue upstream). A filtered or
	// full-page response still absorbs rows -- useful state -- but sets no
	// completeness marker.
	complete := shape.head == "" && len(rows) < shape.perPage
	absorbOwner, absorbRepo := owner, repo
	if len(rows) > 0 {
		absorbOwner, absorbRepo = rows[0].Owner, rows[0].Repo
	}
	if err := h.store.AbsorbPullsList(r.Context(), absorbOwner, absorbRepo, rows, labelsByPR, complete, now, pullsListCacheTTL); err != nil {
		slog.Warn("pulls list absorb failed", "owner", owner, "repo", repo, "error", err)
	}
	h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispMiss, resp.StatusCode)
	h.servePullsList(w, r, rows, labelsByPR, false)
}

// servePullsList rebuilds and writes the trimmed list. Hit and miss serve the
// same shape; a miss keeps GitHub's own response order, a hit serves
// newest-created first (GitHub's default sort).
func (h *handlers) servePullsList(w http.ResponseWriter, r *http.Request, rows []dbgen.PullRequest, labelsByPR map[int64][]dbgen.PrLabel, hit bool) {
	items := make([]pullListItemJSON, 0, len(rows))
	for _, pr := range rows {
		items = append(items, renderPullListItem(pr, labelsByPR[pr.Number]))
	}
	body, err := marshalTrimmed(items)
	if err != nil {
		slog.Warn("pulls list render failed", "path", r.URL.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hit {
		h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, DispHit)
	}
	writeRebuilt(w, http.StatusOK, body, hit)
}

// allRestComplete reports whether every row can be rebuilt as a REST response
// (GraphQL-sourced rows cannot; SetRepoPRs also clears the list marker, so
// this is belt and braces).
func allRestComplete(rows []dbgen.PullRequest) bool {
	for _, pr := range rows {
		if !ghdata.PRRestComplete(pr) {
			return false
		}
	}
	return true
}

// ---- GET /repos/{owner}/{repo}/pulls/{number} ----

// cachedPull serves a single OPEN PR from state when the row is
// rest-complete AND its mergeable is known; everything else -- unknown or
// null mergeable, closed or unknown PRs, non-numeric path segments like
// /pulls/comments, query params, non-default Accept -- misses or passes
// through. A fetched open PR is absorbed (including GitHub's computed
// mergeable, null and all); a fetched closed PR deletes any cached row and
// replays verbatim (the cache retains open PRs only).
func (h *handlers) cachedPull(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	numStr := chi.URLParam(r, "number")

	number, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil || number <= 0 || !acceptsDefaultJSON(r) || r.URL.RawQuery != "" {
		h.ghProxy.ServeHTTP(w, r)
		return
	}

	if pr, labels, ok, err := h.store.RestSinglePull(r.Context(), owner, repo, number); err != nil {
		slog.Warn("single PR cache read failed", "owner", owner, "repo", repo, "number", number, "error", err)
	} else if ok && ghdata.PRRestComplete(pr) && mergeableKnown(pr) {
		h.serveSinglePull(w, r, pr, labels, true)
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
		// Closed/merged: the cache retains open PRs only. Drop any stale row
		// and hand GitHub's own answer through, unstored.
		if err := h.store.DeletePullForActor(r.Context(), pr.Owner, pr.Repo, pr.Number); err != nil {
			slog.Warn("delete closed PR row failed", "owner", pr.Owner, "repo", pr.Repo, "number", pr.Number, "error", err)
		}
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.AbsorbSinglePull(r.Context(), pr, labels); err != nil {
		slog.Warn("single PR absorb failed", "owner", pr.Owner, "repo", pr.Repo, "number", pr.Number, "error", err)
	}
	h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispMiss, resp.StatusCode)
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
		h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, DispHit)
	}
	writeRebuilt(w, http.StatusOK, body, hit)
}

// mergeableKnown reports whether the row's mergeable is a resolved answer.
// NULL (unresolved / un-resolved by a branch push) and the GraphQL "UNKNOWN"
// both gate the single-PR route to a miss so pr-minder's resolve-poll always
// reaches GitHub.
func mergeableKnown(pr dbgen.PullRequest) bool {
	return pr.Mergeable.Valid &&
		(pr.Mergeable.String == "MERGEABLE" || pr.Mergeable.String == "CONFLICTING")
}

// ---- absorbing REST PR bodies ----

// restPRJSON is the upstream shape both PR routes parse: the "simple PR" of a
// list item, plus the merge-status fields only the single-PR response carries.
type restPRJSON struct {
	Number         int64   `json:"number"`
	NodeID         string  `json:"node_id"`
	State          string  `json:"state"`
	Title          string  `json:"title"`
	Body           *string `json:"body"`
	Draft          bool    `json:"draft"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
	Mergeable      *bool   `json:"mergeable"` // single-PR responses only
	Additions      *int64  `json:"additions"` // single-PR responses only
	Deletions      *int64  `json:"deletions"` // single-PR responses only
	HTMLURL        string  `json:"html_url"`
	User           *struct {
		Login     string `json:"login"`
		Type      string `json:"type"`
		AvatarURL string `json:"avatar_url"`
		HTMLURL   string `json:"html_url"`
	} `json:"user"`
	Labels []struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	} `json:"labels"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repo"`
	} `json:"base"`
	AutoMerge *struct {
		MergeMethod string `json:"merge_method"`
	} `json:"auto_merge"`
	RequestedReviewers []json.RawMessage `json:"requested_reviewers"`
	RequestedTeams     []json.RawMessage `json:"requested_teams"`
}

// absorbPullsListBody parses a /pulls 200 array into storable rows + labels.
// Reports false -- serve verbatim, store nothing -- for any other status or
// any item the model cannot hold.
func absorbPullsListBody(owner, repo string, status int, body []byte) ([]dbgen.PullRequest, map[int64][]dbgen.PrLabel, bool) {
	if status != http.StatusOK {
		return nil, nil, false
	}
	var raw []restPRJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, false
	}
	rows := make([]dbgen.PullRequest, 0, len(raw))
	labelsByPR := make(map[int64][]dbgen.PrLabel, len(raw))
	for _, item := range raw {
		pr, labels, ok := absorbRestPR(owner, repo, item)
		if !ok {
			return nil, nil, false
		}
		rows = append(rows, pr)
		labelsByPR[pr.Number] = labels
	}
	return rows, labelsByPR, true
}

// absorbRestPR converts one REST PR object into a pull_requests row (+ label
// rows), reporting false when required fields are missing. owner/repo come
// from the response's own base.repo (canonical casing, so rows collide with
// webhook/GraphQL-written ones); the URL values are only the fallback.
func absorbRestPR(urlOwner, urlRepo string, p restPRJSON) (dbgen.PullRequest, []dbgen.PrLabel, bool) {
	if p.Number <= 0 || p.NodeID == "" || p.User == nil || p.User.Login == "" ||
		p.Head.Ref == "" || p.Head.SHA == "" || p.Base.Ref == "" || p.Base.SHA == "" ||
		p.CreatedAt == "" || p.UpdatedAt == "" {
		return dbgen.PullRequest{}, nil, false
	}
	var state string
	switch p.State {
	case "open":
		state = "OPEN"
	case "closed":
		state = "CLOSED"
	default:
		return dbgen.PullRequest{}, nil, false
	}
	owner, repo := urlOwner, urlRepo
	if p.Base.Repo != nil && p.Base.Repo.Owner.Login != "" && p.Base.Repo.Name != "" {
		owner, repo = p.Base.Repo.Owner.Login, p.Base.Repo.Name
	}
	pr := dbgen.PullRequest{
		Owner:        owner,
		Repo:         repo,
		Number:       p.Number,
		Title:        p.Title,
		Url:          p.HTMLURL,
		IsDraft:      boolToInt64(p.Draft),
		State:        state,
		CreatedAt:    normalizeRESTTime(p.CreatedAt),
		UpdatedAt:    normalizeRESTTime(p.UpdatedAt),
		NodeID:       sql.NullString{String: p.NodeID, Valid: true},
		AuthorLogin:  sql.NullString{String: p.User.Login, Valid: true},
		AuthorType:   nullableStr(p.User.Type),
		AuthorAvatar: nullableStr(p.User.AvatarURL),
		AuthorUrl:    nullableStr(p.User.HTMLURL),
		HeadRefName:  sql.NullString{String: p.Head.Ref, Valid: true},
		HeadRefOid:   sql.NullString{String: p.Head.SHA, Valid: true},
		BaseRefName:  sql.NullString{String: p.Base.Ref, Valid: true},
		BaseRefOid:   sql.NullString{String: p.Base.SHA, Valid: true},
		ReviewRequestCount: sql.NullInt64{
			Int64: int64(len(p.RequestedReviewers) + len(p.RequestedTeams)), Valid: true,
		},
	}
	if p.Body != nil {
		pr.Body = sql.NullString{String: *p.Body, Valid: true}
	}
	if p.Head.Repo != nil {
		pr.HeadRepoFullName = nullableStr(p.Head.Repo.FullName)
	}
	if p.AutoMerge != nil {
		pr.AutoMergeMethod = nullableStr(p.AutoMerge.MergeMethod)
	}
	if p.MergeCommitSHA != nil {
		pr.MergeCommitSha = nullableStr(*p.MergeCommitSHA)
	}
	if p.Mergeable != nil {
		m := "CONFLICTING"
		if *p.Mergeable {
			m = "MERGEABLE"
		}
		pr.Mergeable = sql.NullString{String: m, Valid: true}
	}
	if p.Additions != nil {
		pr.Additions = sql.NullInt64{Int64: *p.Additions, Valid: true}
	}
	if p.Deletions != nil {
		pr.Deletions = sql.NullInt64{Int64: *p.Deletions, Valid: true}
	}
	labels := make([]dbgen.PrLabel, 0, len(p.Labels))
	for _, l := range p.Labels {
		labels = append(labels, dbgen.PrLabel{
			Owner: owner, Repo: repo, PrNumber: p.Number, Name: l.Name, Color: l.Color,
		})
	}
	return pr, labels, true
}

// ---- rebuilding trimmed PR bodies ----

// The rebuilt shapes: GitHub's list/single PR fields that carry STATE, minus
// every URL field and the untracked clutter (milestone, assignees, locked,
// author_association, ...). A superset of every field pr-minder and the
// pr-minder-reconcile hook read off mirror-served PR objects: number, state,
// draft, title, body, node_id, user.login/.type, labels[].name/.color,
// head.ref/.sha/.repo.full_name, base.ref/.sha, auto_merge, merge_commit_sha,
// created_at/updated_at, and (single) mergeable/merged.
type pullUserJSON struct {
	Login string `json:"login"`
	Type  string `json:"type,omitempty"`
}

type pullLabelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type pullRepoRefJSON struct {
	FullName string `json:"full_name"`
}

type pullHeadJSON struct {
	Ref  string           `json:"ref"`
	SHA  string           `json:"sha"`
	Repo *pullRepoRefJSON `json:"repo"` // null when the head repo is gone
}

type pullBaseJSON struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type pullAutoMergeJSON struct {
	MergeMethod string `json:"merge_method"`
}

type pullListItemJSON struct {
	NodeID         string             `json:"node_id"`
	Number         int64              `json:"number"`
	State          string             `json:"state"`
	Title          string             `json:"title"`
	User           pullUserJSON       `json:"user"`
	Body           *string            `json:"body"`
	Labels         []pullLabelJSON    `json:"labels"`
	CreatedAt      string             `json:"created_at"`
	UpdatedAt      string             `json:"updated_at"`
	MergeCommitSHA *string            `json:"merge_commit_sha"`
	Draft          bool               `json:"draft"`
	AutoMerge      *pullAutoMergeJSON `json:"auto_merge"`
	Head           pullHeadJSON       `json:"head"`
	Base           pullBaseJSON       `json:"base"`
}

type pullSingleJSON struct {
	pullListItemJSON
	Merged    bool  `json:"merged"`
	Mergeable *bool `json:"mergeable"`
}

func renderPullListItem(pr dbgen.PullRequest, labels []dbgen.PrLabel) pullListItemJSON {
	item := pullListItemJSON{
		NodeID:    pr.NodeID.String,
		Number:    pr.Number,
		State:     strings.ToLower(pr.State),
		Title:     pr.Title,
		User:      pullUserJSON{Login: pr.AuthorLogin.String, Type: pr.AuthorType.String},
		Labels:    make([]pullLabelJSON, 0, len(labels)),
		CreatedAt: pr.CreatedAt,
		UpdatedAt: pr.UpdatedAt,
		Draft:     pr.IsDraft != 0,
		Head:      pullHeadJSON{Ref: pr.HeadRefName.String, SHA: pr.HeadRefOid.String},
		Base:      pullBaseJSON{Ref: pr.BaseRefName.String, SHA: pr.BaseRefOid.String},
	}
	if pr.Body.Valid {
		item.Body = &pr.Body.String
	}
	if pr.MergeCommitSha.Valid && pr.MergeCommitSha.String != "" {
		item.MergeCommitSHA = &pr.MergeCommitSha.String
	}
	if pr.AutoMergeMethod.Valid && pr.AutoMergeMethod.String != "" {
		item.AutoMerge = &pullAutoMergeJSON{MergeMethod: pr.AutoMergeMethod.String}
	}
	if pr.HeadRepoFullName.Valid && pr.HeadRepoFullName.String != "" {
		item.Head.Repo = &pullRepoRefJSON{FullName: pr.HeadRepoFullName.String}
	}
	for _, l := range labels {
		item.Labels = append(item.Labels, pullLabelJSON{Name: l.Name, Color: l.Color})
	}
	return item
}

func renderSinglePull(pr dbgen.PullRequest, labels []dbgen.PrLabel) pullSingleJSON {
	out := pullSingleJSON{pullListItemJSON: renderPullListItem(pr, labels)}
	// Only OPEN PRs are ever rebuilt (hit gate + absorb gate), so merged is
	// false by definition.
	switch pr.Mergeable.String {
	case "MERGEABLE":
		v := true
		out.Mergeable = &v
	case "CONFLICTING":
		v := false
		out.Mergeable = &v
	}
	return out
}

// ---- GET /repos/{owner}/{repo}/installation ----

// cachedRepoInstallation caches the App-level repo-installation lookup. Like
// the token-mint route it sits OUTSIDE requireAuth (its bearer is a GitHub
// App JWT, which cannot resolve GET /user): the handler verifies the JWT
// itself and partitions by the verified app id. Unverifiable callers forward
// unchanged, uncached (GitHub answers them itself).
func (h *handlers) cachedRepoInstallation(w http.ResponseWriter, r *http.Request) {
	jwt := bearerToken(r)
	if jwt == "" {
		h.ghProxy.ServeHTTP(w, r) // the proxy 401s tokenless requests
		return
	}
	ident, err := h.gh.VerifyAppIdentity(r.Context(), jwt)
	if err != nil {
		h.ghProxy.ServeHTTP(w, r)
		return
	}
	if !acceptsDefaultJSON(r) || r.URL.RawQuery != "" {
		h.ghProxy.ServeHTTP(w, r)
		return
	}
	actorKey := fmt.Sprintf("app:%d", ident.ID)
	ctx := actor.WithActor(r.Context(), actorKey)
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))

	now := time.Now()
	if c, ok, err := h.store.GetCachedRepoInstallation(ctx, owner, repo, now); err == nil && ok {
		h.reqlog.record(actorKey, r.Method, r.URL.Path, DispHit)
		h.serveRepoInstallation(w, c, true)
		return
	} else if err != nil {
		slog.Warn("repo installation cache read failed", "owner", owner, "repo", repo, "error", err)
	}

	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	c, absorbed := absorbRepoInstallation(owner, repo, resp.StatusCode, body)
	if overflow || !absorbed {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedRepoInstallation(ctx, c, now, repoInstallationCacheTTL); err != nil {
		slog.Warn("repo installation cache write failed", "owner", owner, "repo", repo, "error", err)
	}
	h.reqlog.recordStatus(actorKey, r.Method, r.URL.Path, DispMiss, resp.StatusCode)
	h.serveRepoInstallation(w, c, false)
}

// repoInstallationJSON is the trimmed rebuild: GitHub's installation object
// minus every *_url field and the untracked clutter (permissions, events,
// timestamps). pr-minder reads only .id.
type repoInstallationJSON struct {
	ID                  int64                  `json:"id"`
	Account             repoInstallAccountJSON `json:"account"`
	RepositorySelection string                 `json:"repository_selection,omitempty"`
	AppID               int64                  `json:"app_id,omitempty"`
	AppSlug             string                 `json:"app_slug,omitempty"`
	TargetType          string                 `json:"target_type,omitempty"`
}

type repoInstallAccountJSON struct {
	Login string `json:"login"`
	Type  string `json:"type,omitempty"`
}

func (h *handlers) serveRepoInstallation(w http.ResponseWriter, c ghdata.CachedRepoInstallation, hit bool) {
	body, err := marshalTrimmed(repoInstallationJSON{
		ID:                  c.InstallationID,
		Account:             repoInstallAccountJSON{Login: c.AccountLogin, Type: c.AccountType},
		RepositorySelection: c.RepositorySelection,
		AppID:               c.AppID,
		AppSlug:             c.AppSlug,
		TargetType:          c.TargetType,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeRebuilt(w, http.StatusOK, body, hit)
}

// absorbRepoInstallation parses an upstream repo-installation response. Only
// a well-formed 200 is absorbed; 404 ("app not installed on this repo") is
// replayed unstored -- the app can be installed a moment later.
func absorbRepoInstallation(owner, repo string, status int, body []byte) (ghdata.CachedRepoInstallation, bool) {
	if status != http.StatusOK {
		return ghdata.CachedRepoInstallation{}, false
	}
	var g struct {
		ID      int64 `json:"id"`
		Account *struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
		RepositorySelection string `json:"repository_selection"`
		AppID               int64  `json:"app_id"`
		AppSlug             string `json:"app_slug"`
		TargetType          string `json:"target_type"`
	}
	if err := json.Unmarshal(body, &g); err != nil || g.ID <= 0 {
		return ghdata.CachedRepoInstallation{}, false
	}
	c := ghdata.CachedRepoInstallation{
		Owner: owner, Repo: repo, InstallationID: g.ID,
		RepositorySelection: g.RepositorySelection,
		AppID:               g.AppID, AppSlug: g.AppSlug, TargetType: g.TargetType,
	}
	if g.Account != nil {
		c.AccountLogin, c.AccountType = g.Account.Login, g.Account.Type
	}
	return c, true
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

// normalizeRESTTime folds a REST timestamp to the fixed-width UTC RFC3339
// form the schema stores (mirrors the webhook package's normaliseTime).
func normalizeRESTTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339)
}
