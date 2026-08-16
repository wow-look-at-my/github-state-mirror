package api

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/go-containers/set"
)

// The dashboard JSON API: /api/me, /api/jobs, /api/requests, and the cache
// stats the Principals tab renders. The page shell, asset embedding, and the
// OAuth sign-in flow live in dashboard.go.

// ---- dashboard API ----

type meResponse struct {
	Authenticated   bool   `json:"authenticated"`
	LoginConfigured bool   `json:"login_configured"`
	Login           string `json:"login,omitempty"`
	IsAdmin         bool   `json:"is_admin"`
}

func (d *dashboard) handleMe(w http.ResponseWriter, r *http.Request) {
	resp := meResponse{LoginConfigured: d.auth.Configured()}
	if login, ok := d.auth.Session(r); ok {
		resp.Authenticated = true
		resp.Login = login
		resp.IsAdmin = d.auth.IsAdmin(login)
	}
	writeJSON(w, resp)
}

type jobsResponse struct {
	Jobs []ghdata.WorkflowJob `json:"jobs"`
}

// Bounds for the /api/jobs limit query param.
const (
	jobsDefaultLimit = 100
	jobsMaxLimit     = 500
)

// handleJobs returns recent GitHub Actions jobs recorded from workflow_job
// webhooks: running jobs first (newest started first), then completed jobs
// (newest completed first). Like the webhook log, the job table is global (it
// spans every repo/tenant), so it is restricted to admins. `limit` caps the
// row count (default 100, max 500).
func (d *dashboard) handleJobs(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	limit := int64(jobsDefaultLimit)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			http.Error(w, "invalid 'limit' query parameter", http.StatusBadRequest)
			return
		}
		limit = min(n, jobsMaxLimit)
	}
	jobs, err := d.store.RecentWorkflowJobs(r.Context(), limit)
	if err != nil {
		slog.Warn("list workflow jobs failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if jobs == nil {
		jobs = []ghdata.WorkflowJob{}
	}
	writeJSON(w, jobsResponse{Jobs: jobs})
}

// handleRequests returns recent data-API requests and their cache disposition
// (hit / miss / passthrough). Like the webhook log it spans every actor/tenant,
// so — consistent with the admin-only "all scopes" view — it is admin-only.
// The payload also carries the SQLite database's on-disk size (statted fresh
// per request) so the tab's summary shows the cache's real footprint.
func (d *dashboard) handleRequests(w http.ResponseWriter, r *http.Request) {
	login, ok := d.auth.Session(r)
	if !ok {
		http.Error(w, "unauthorized: sign in first", http.StatusUnauthorized)
		return
	}
	if !d.auth.IsAdmin(login) {
		http.Error(w, "forbidden: admin only", http.StatusForbidden)
		return
	}
	snap := d.reqlog.snapshot(200)
	snap.DBSizeBytes, snap.DBWALSizeBytes = dbFileSizes(d.dbPath)
	writeJSON(w, snap)
}

// dbFileSizes reports the on-disk sizes of the SQLite database file and its
// -wal sidecar (bytes written ahead of a checkpoint — part of the real
// footprint). One os.Stat each — cheap enough per request, no caching. A
// missing file or stat error yields 0 (the field is omitted from the JSON),
// never a failure: the request log must render regardless.
func dbFileSizes(path string) (db, wal int64) {
	if path == "" {
		return 0, 0
	}
	if fi, err := os.Stat(path); err == nil {
		db = fi.Size()
	}
	if fi, err := os.Stat(path + "-wal"); err == nil {
		wal = fi.Size()
	}
	return db, wal
}

type kindFreshness struct {
	Kind        string           `json:"kind"`
	States      map[string]int64 `json:"states"`
	LastFetched string           `json:"last_fetched,omitempty"`
	// Error is the captured failure reason for a resource of this kind currently
	// in the error state (ErrorKey identifies which one), so the dashboard can
	// show *why* a kind is erroring, not just the count.
	Error    string `json:"error,omitempty"`
	ErrorKey string `json:"error_key,omitempty"`
}

type recentRefresh struct {
	Kind      string `json:"kind"`
	Key       string `json:"key"`
	Trigger   string `json:"trigger"`
	StartedAt string `json:"started_at"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// principalStats is one principal's reveal-layer standing: who they are, how
// many repos they hold live grants for, and how fresh their org syncs are.
type principalStats struct {
	Principal   string          `json:"principal"`    // short (display)
	PrincipalID string          `json:"principal_id"` // full key (for admin views)
	Login       string          `json:"login"`
	IsSelf      bool            `json:"is_self"`
	LastSeen    string          `json:"last_seen,omitempty"`
	LiveGrants  int64           `json:"live_grants"`
	Kinds       []kindFreshness `json:"kinds"`
	Recent      []recentRefresh `json:"recent,omitempty"`
}

type cacheResponse struct {
	Login   string `json:"login"`
	IsAdmin bool   `json:"is_admin"`
	Scope   string `json:"scope"`
	// Totals are the GLOBAL truth store's row counts -- one cache, one truth.
	Totals ghdata.DataCounts `json:"totals"`
	// Principals lists reveal-layer principals: the signed-in user's own on
	// the "mine" view, every known one on the admin "all" view.
	PrincipalCount int              `json:"principal_count"`
	Principals     []principalStats `json:"principals"`
	// Truth is the freshness of shared global truth markers (the 'global'
	// actor rows, e.g. repo_pulls completeness is tracked elsewhere; this
	// carries whatever global markers exist).
	Truth []kindFreshness `json:"truth,omitempty"`
}

const unknownLogin = "(unknown)"

// principalInput is one principal to summarize, with its (possibly unknown)
// identity.
type principalInput struct {
	principal string
	login     string
	lastSeen  string
}

func (d *dashboard) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	login, ok := d.auth.Session(r)
	if !ok {
		http.Error(w, "unauthorized: sign in first", http.StatusUnauthorized)
		return
	}
	isAdmin := d.auth.IsAdmin(login)

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "mine"
	}
	if scope == "all" && !isAdmin {
		http.Error(w, "forbidden: admin only", http.StatusForbidden)
		return
	}

	ctx := r.Context()
	totals, err := d.store.GlobalDataCounts(ctx)
	if err != nil {
		slog.Warn("global data counts failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	identities, err := d.store.ListActorIdentities(ctx)
	if err != nil {
		slog.Warn("list principal identities failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	inputs := d.collectInputs(ctx, scope, login, identities)

	resp := cacheResponse{Login: login, IsAdmin: isAdmin, Scope: scope, Totals: totals, PrincipalCount: len(inputs)}
	detailed := scope != "all" // recent activity only on the focused (mine) view
	for _, in := range inputs {
		p, err := d.buildPrincipal(ctx, in, login, detailed)
		if err != nil {
			slog.Warn("build principal failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		resp.Principals = append(resp.Principals, p)
	}
	if resp.Principals == nil {
		resp.Principals = []principalStats{}
	}
	// Shared global truth markers (fetched-for-everyone resources).
	if truthFresh, err := d.store.FreshnessByKind(ctx, "global"); err == nil {
		truthErrs, _ := d.store.ErrorMessagesByKind(ctx, "global")
		resp.Truth = groupKinds(truthFresh, truthErrs)
	}
	writeJSON(w, resp)
}

// collectInputs returns the principals to summarize for the requested scope,
// sorted for stable display.
func (d *dashboard) collectInputs(ctx context.Context, scope, login string, identities []dbgen.ActorIdentity) []principalInput {
	if scope != "all" {
		var inputs []principalInput
		for _, id := range identities {
			if id.Login == login {
				inputs = append(inputs, principalInput{principal: id.Actor, login: id.Login, lastSeen: id.LastSeen})
			}
		}
		return inputs
	}

	// Admin "all": every identity, plus any principal with freshness metadata
	// but no identity row (e.g. the background app-installation sessions).
	seen := set.New[string](len(identities))
	inputs := make([]principalInput, 0, len(identities))
	for _, id := range identities {
		inputs = append(inputs, principalInput{principal: id.Actor, login: id.Login, lastSeen: id.LastSeen})
		seen.Add(id.Actor)
	}
	if known, err := d.store.KnownPrincipals(ctx); err != nil {
		slog.Warn("list known principals failed", "error", err)
	} else {
		for _, a := range known {
			if !seen.Contains(a) {
				inputs = append(inputs, principalInput{principal: a, login: unknownLogin})
			}
		}
	}
	// Known logins first (case-insensitive), unknowns last, then by principal.
	sort.SliceStable(inputs, func(i, j int) bool {
		ui, uj := inputs[i].login == unknownLogin, inputs[j].login == unknownLogin
		if ui != uj {
			return uj // a known login sorts before an unknown one
		}
		li, lj := strings.ToLower(inputs[i].login), strings.ToLower(inputs[j].login)
		if li != lj {
			return li < lj
		}
		return inputs[i].principal < inputs[j].principal
	})
	return inputs
}

func (d *dashboard) buildPrincipal(ctx context.Context, in principalInput, selfLogin string, detailed bool) (principalStats, error) {
	grants, err := d.store.CountLiveGrants(ctx, in.principal, time.Now())
	if err != nil {
		return principalStats{}, err
	}
	fresh, err := d.store.FreshnessByKind(ctx, in.principal)
	if err != nil {
		return principalStats{}, err
	}
	errs, err := d.store.ErrorMessagesByKind(ctx, in.principal)
	if err != nil {
		return principalStats{}, err
	}

	p := principalStats{
		Principal:   shortFingerprint(in.principal),
		PrincipalID: in.principal,
		Login:       in.login,
		IsSelf:      in.login == selfLogin && in.login != unknownLogin,
		LastSeen:    in.lastSeen,
		LiveGrants:  grants,
		Kinds:       groupKinds(fresh, errs),
	}
	if detailed {
		logs, err := d.store.RecentRefreshes(ctx, in.principal, 12)
		if err != nil {
			return principalStats{}, err
		}
		p.Recent = toRecent(logs)
	}
	return p, nil
}

// groupKinds folds per-(kind,state) rows into one entry per resource kind, with
// a map of state -> count and the most recent fetch time across that kind. When
// a kind has an errored resource, the first captured error message (and its key)
// is attached so the dashboard can show why it failed.
func groupKinds(rows []dbgen.ActorFreshnessByKindRow, errRows []dbgen.ActorErrorMessagesByKindRow) []kindFreshness {
	order := make([]string, 0)
	byKind := make(map[string]*kindFreshness)
	for _, row := range rows {
		kf, ok := byKind[row.ResourceKind]
		if !ok {
			kf = &kindFreshness{Kind: row.ResourceKind, States: map[string]int64{}}
			byKind[row.ResourceKind] = kf
			order = append(order, row.ResourceKind)
		}
		kf.States[row.FetchState] += row.Count
		// last_fetched_at is stored as RFC3339 UTC, so lexical max == latest.
		if lf := asTimeString(row.LastFetched); lf > kf.LastFetched {
			kf.LastFetched = lf
		}
	}
	// Attach the first error message per kind (rows are ordered by kind, key).
	for _, e := range errRows {
		kf, ok := byKind[e.ResourceKind]
		if !ok || kf.Error != "" {
			continue
		}
		kf.Error = e.ErrorMessage.String
		kf.ErrorKey = e.ResourceKey
	}
	out := make([]kindFreshness, 0, len(order))
	for _, k := range order {
		out = append(out, *byKind[k])
	}
	return out
}

func toRecent(logs []dbgen.CacheRefreshLog) []recentRefresh {
	out := make([]recentRefresh, 0, len(logs))
	for _, l := range logs {
		status := "running"
		if l.CompletedAt.Valid {
			if l.Success.Valid && l.Success.Int64 == 1 {
				status = "success"
			} else {
				status = "error"
			}
		}
		out = append(out, recentRefresh{
			Kind:      l.ResourceKind,
			Key:       l.ResourceKey,
			Trigger:   l.TriggeredBy,
			StartedAt: l.StartedAt,
			Status:    status,
			// Surface the captured failure reason (cache_refresh_log.error_message)
			// so the dashboard can show *why* a refresh errored, not just that it did.
			Error: l.ErrorMessage.String,
		})
	}
	return out
}

// asTimeString coerces sqlc's interface{} MAX() result (string, []byte, or nil)
// into a string.
func asTimeString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}

// shortFingerprint abbreviates an actor for display: opaque hex token
// fingerprints shorten to 12 chars, structured actors ("user:<id>",
// "app:<id>", "app-installation:<id>") are shown whole.
func shortFingerprint(fp string) string { return actor.Short(fp) }
