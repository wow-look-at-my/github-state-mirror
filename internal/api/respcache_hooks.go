package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The cached webhook CONFIGURATION listings (tier of the cache contract):
//
//	GET /repos/{owner}/{repo}/hooks
//	GET /orgs/{org}/hooks
//
// Keyed by the bearer's fingerprint: an admin-only read, so a global row

const (
	// hooksCacheTTL stays short: a reconciler working from a stale listing could create a duplicate webhook.
	hooksCacheTTL = 5 * time.Minute

	hooksDefaultPerPage = 30
	// hooksMaxCachedPage caps modeled pages; deeper pagination passes through.
	hooksMaxCachedPage = 10
)

// cachedRepoHooks serves page of a repository's webhook configuration.
func (h *handlers) cachedRepoHooks(w http.ResponseWriter, r *http.Request) {
	h.serveHooks(w, r, ghdata.RepoHooksTarget(chi.URLParam(r, "owner"), chi.URLParam(r, "repo")))
}

// cachedOrgHooks serves page of an organization's webhook configuration.
func (h *handlers) cachedOrgHooks(w http.ResponseWriter, r *http.Request) {
	h.serveHooks(w, r, ghdata.OrgHooksTarget(chi.URLParam(r, "org")))
}

// serveHooks is the shared flow. The routes differ only in the target they
// name: the request shape, the rebuild, the key, and the invalidation are
// identical, because GitHub answers both with the same hook object.
func (h *handlers) serveHooks(w http.ResponseWriter, r *http.Request, target ghdata.HooksTarget) {
	token := bearerToken(r)
	if token == "" {
		// requireAuth already rejects these; belt-and-braces against an empty credential key.
		h.passthrough(w, r, PassIdentity)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	perPage, page, ok := parseHooksShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}

	fp := ghclient.Fingerprint(token)
	now := time.Now()
	if doc, status, ok, err := h.store.GetCachedHooks(r.Context(), fp, target, perPage, page, now); err != nil {
		slog.Warn("hooks cache read failed", "scope", target.Scope, "owner", target.Owner, "repo", target.Repo, "error", err)
	} else if ok {
		h.reqlog.observe(r, DispHit)
		writeRebuilt(w, int(status), []byte(doc), true)
		return
	}

	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	doc, absorbed := absorbHooks(resp.StatusCode, body)
	if overflow || !absorbed {
		// //5xx and unmodeled shapes relay unstored; a permission grant can change with no event reaching the mirror.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedHooks(r.Context(), fp, target, perPage, page, http.StatusOK, doc, now, hooksCacheTTL); err != nil {
		slog.Warn("hooks cache write failed", "scope", target.Scope, "owner", target.Owner, "repo", target.Repo, "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// A single hook's configuration. Same subject as the listing, so a write
// flushes both.
func (h *handlers) cachedRepoHook(w http.ResponseWriter, r *http.Request) {
	h.serveHook(w, r, ghdata.RepoHooksTarget(chi.URLParam(r, "owner"), chi.URLParam(r, "repo")))
}

func (h *handlers) cachedOrgHook(w http.ResponseWriter, r *http.Request) {
	h.serveHook(w, r, ghdata.OrgHooksTarget(chi.URLParam(r, "org")))
}

func (h *handlers) serveHook(w http.ResponseWriter, r *http.Request, target ghdata.HooksTarget) {
	token := bearerToken(r)
	if token == "" {
		h.passthrough(w, r, PassIdentity)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	if len(r.URL.Query()) > 0 {
		// No query parameter changes this answer, so any is unmodeled.
		h.passthrough(w, r, PassQuery)
		return
	}
	hookID, err := strconv.ParseInt(chi.URLParam(r, "hook_id"), 10, 64)
	if err != nil || hookID <= 0 {
		h.passthrough(w, r, PassPath)
		return
	}
	target = target.Hook(hookID)

	fp := ghclient.Fingerprint(token)
	now := time.Now()
	if doc, status, ok, err := h.store.GetCachedHooks(r.Context(), fp, target, 0, 0, now); err != nil {
		slog.Warn("hook cache read failed", "scope", target.Scope, "owner", target.Owner, "repo", target.Repo, "error", err)
	} else if ok {
		h.reqlog.observe(r, DispHit)
		writeRebuilt(w, int(status), []byte(doc), true)
		return
	}

	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	doc, status, absorbed := absorbHook(resp.StatusCode, body)
	if overflow || !absorbed {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedHooks(r.Context(), fp, target, 0, 0, int64(status), doc, now, hooksCacheTTL); err != nil {
		slog.Warn("hook cache write failed", "scope", target.Scope, "owner", target.Owner, "repo", target.Repo, "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, status, []byte(doc), false)
}

// writeRepoHooks / writeOrgHooks flush hooks across every credential, before forwarding the write.
// see docs/cache/rest-routes.md
func (h *handlers) writeRepoHooks(w http.ResponseWriter, r *http.Request) {
	h.flushHooksThenForward(w, r, ghdata.RepoHooksTarget(chi.URLParam(r, "owner"), chi.URLParam(r, "repo")))
}

func (h *handlers) writeOrgHooks(w http.ResponseWriter, r *http.Request) {
	h.flushHooksThenForward(w, r, ghdata.OrgHooksTarget(chi.URLParam(r, "org")))
}

func (h *handlers) flushHooksThenForward(w http.ResponseWriter, r *http.Request, target ghdata.HooksTarget) {
	if err := h.store.InvalidateHooksForTarget(r.Context(), target); err != nil {
		slog.Warn("hooks flush on write failed", "scope", target.Scope, "owner", target.Owner, "repo", target.Repo, "error", err)
	}
	h.ghProxy.ServeHTTP(w, r)
}

// parseHooksShape reports the paging shape and whether the cache models it.
// The endpoint takes only per_page/page.
func parseHooksShape(q url.Values) (perPage, page int64, ok bool) {
	perPage, page = hooksDefaultPerPage, 1
	for key, vals := range q {
		if len(vals) != 1 {
			return 0, 0, false
		}
		n, err := strconv.ParseInt(vals[0], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		switch key {
		case "per_page":
			if n < 1 || n > 100 {
				return 0, 0, false
			}
			perPage = n
		case "page":
			if n < 1 || n > hooksMaxCachedPage {
				return 0, 0, false
			}
			page = n
		default:
			return 0, 0, false
		}
	}
	return perPage, page, true
}

// hookJSON is trimmed hook. config.url is a pinned no-URL exception: it is the hook's own destination, not a link into GitHub's API.
// see docs/cache/rest-routes.md
type hookJSON struct {
	ID           int64                 `json:"id"`
	Type         string                `json:"type,omitempty"`
	Name         string                `json:"name"`
	Active       bool                  `json:"active"`
	Events       []string              `json:"events"`
	Config       hookConfigJSON        `json:"config"`
	CreatedAt    string                `json:"created_at"`
	UpdatedAt    string                `json:"updated_at"`
	LastResponse *hookLastResponseJSON `json:"last_response,omitempty"`
}

// hookConfigJSON preserves secret's PRESENCE exactly (a fixed mask means is set); insecure_ssl rides as raw JSON since GitHub documents it as either a string or a number.
type hookConfigJSON struct {
	URL         string          `json:"url,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	InsecureSSL json.RawMessage `json:"insecure_ssl,omitempty"`
	Secret      *string         `json:"secret,omitempty"`
}

// hookLastResponseJSON is honestly TTL-stale: it moves with every delivery, and no webhook names the change.
type hookLastResponseJSON struct {
	Code    *int64  `json:"code"`
	Status  string  `json:"status"`
	Message *string `json:"message"`
}

// rawHook is GitHub's hook object as it arrives, in a listing or alone. Both
// answers trim through this, so neither can describe a hook differently.
type rawHook struct {
	ID     int64    `json:"id"`
	Type   string   `json:"type"`
	Name   string   `json:"name"`
	Active bool     `json:"active"`
	Events []string `json:"events"`
	Config *struct {
		URL         string          `json:"url"`
		ContentType string          `json:"content_type"`
		InsecureSSL json.RawMessage `json:"insecure_ssl"`
		Secret      *string         `json:"secret"`
	} `json:"config"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	LastResponse *struct {
		Code    *int64  `json:"code"`
		Status  string  `json:"status"`
		Message *string `json:"message"`
	} `json:"last_response"`
}

// trim renders the stored shape, reporting false for a hook missing the
// identity fields every answer carries.
func (raw rawHook) trim() (hookJSON, bool) {
	if raw.ID <= 0 || raw.Name == "" {
		return hookJSON{}, false
	}
	item := hookJSON{
		ID: raw.ID, Type: raw.Type, Name: raw.Name, Active: raw.Active,
		Events: raw.Events, CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt,
	}
	if item.Events == nil {
		item.Events = []string{}
	}
	if raw.Config != nil {
		item.Config = hookConfigJSON{
			URL: raw.Config.URL, ContentType: raw.Config.ContentType,
			InsecureSSL: raw.Config.InsecureSSL, Secret: raw.Config.Secret,
		}
	}
	if raw.LastResponse != nil {
		item.LastResponse = &hookLastResponseJSON{
			Code: raw.LastResponse.Code, Status: raw.LastResponse.Status,
			Message: raw.LastResponse.Message,
		}
	}
	return item, true
}

// absorbHooks parses a hooks listing into the trimmed document, rendered
//
//	here so hit and miss serve identical bytes. The body must be an ARRAY
//
// and every entry must carry a positive id and a name; an empty array (no
// hooks configured, or a page past the end) is a valid cacheable answer -- it
// IS what a reconciliation sweep is asking.
func absorbHooks(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return "", false
	}
	var raw []rawHook
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return "", false
	}
	out := make([]hookJSON, 0, len(raw))
	for _, hook := range raw {
		item, ok := hook.trim()
		if !ok {
			return "", false
		}
		out = append(out, item)
	}
	rendered, err := marshalTrimmed(out)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}

// absorbHook parses a single hook answer into the document and the status to
// store it under. A not-found verdict is authoritative, so it is stored. A
// forbidden one is not, because a permission changes with no event.
func absorbHook(status int, body []byte) (string, int, bool) {
	switch status {
	case http.StatusNotFound:
		doc, err := marshalTrimmed(notFoundJSON{Message: upstreamErrorMessage(body), Status: "404"})
		if err != nil {
			return "", 0, false
		}
		return string(doc), http.StatusNotFound, true
	case http.StatusOK:
	default:
		return "", 0, false
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", 0, false
	}
	var raw rawHook
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return "", 0, false
	}
	item, ok := raw.trim()
	if !ok {
		return "", 0, false
	}
	rendered, err := marshalTrimmed(item)
	if err != nil {
		return "", 0, false
	}
	return string(rendered), http.StatusOK, true
}
