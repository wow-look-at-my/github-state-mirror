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

// The cached webhook CONFIGURATION listings (tier 2 of the cache contract):
//
//	GET /repos/{owner}/{repo}/hooks
//	GET /orgs/{org}/hooks
//
// Together the largest genuinely unrouted slice of the request log after the
// runs listing: 6045 and 2016 forwards in one process, a fleet sweep asking
// "is our hook on this repo / this org" over and over.
//
// KEYED BY THE CREDENTIAL, and here that is a security decision rather than a
// correctness one. These are ADMIN-only reads: GitHub refuses them to a caller
// who can merely READ the repository. The reveal layer proves exactly that
// READ access -- and its public fast path admits ANY authenticated principal
// without asking GitHub anything -- so a global row behind the ordinary gate
// would hand a read-only caller the repo's webhook endpoints. A row keyed by
// the bearer's fingerprint is self-gating instead: it can only ever be
// replayed to the exact credential GitHub already answered it for, which needs
// no new authorization machinery to be correct.
//
// What that costs is sharing: two credentials asking about the same repo each
// pay their own fetch. A GLOBAL row would need an admin oracle
// (`permissions.admin` on the repository object, or the installation's own
// permission set) -- docs/cache/rest-routes.md carries that design and, more
// importantly, the arithmetic that decides whether it would actually pay for
// this fleet's traffic. It is not obvious that it would.
//
// STALENESS, stated plainly: the TTL is the primary bound, not a backstop.
// GitHub's `meta` event looks like the invalidation signal and is NOT one --
// it is delivered only to the webhook being deleted, so another hook's
// deletion is something the mirror never hears about. What the mirror does see
// is a write it PROXIES on these same paths (flushed before forwarding, across
// every credential, since one caller's hook change moves everyone's answer)
// and `repository` events. A change made in the UI, or by a client that does
// not go through the mirror, is invisible until the TTL -- which is why it is
// minutes.

const (
	// hooksCacheTTL is the PRIMARY bound on a row: see the staleness note
	// above. A hook reconciler working from a stale listing could create a
	// duplicate webhook, so this stays short even though the mirror flushes
	// on every write it proxies.
	hooksCacheTTL = 5 * time.Minute

	hooksDefaultPerPage = 30
	// hooksMaxCachedPage caps the modeled pages; deeper pagination passes
	// through. No repo or org in this fleet has more than a handful of hooks.
	hooksMaxCachedPage = 10
)

// cachedRepoHooks serves one page of a repository's webhook configuration.
func (h *handlers) cachedRepoHooks(w http.ResponseWriter, r *http.Request) {
	h.serveHooks(w, r, ghdata.RepoHooksTarget(chi.URLParam(r, "owner"), chi.URLParam(r, "repo")))
}

// cachedOrgHooks serves one page of an organization's webhook configuration.
func (h *handlers) cachedOrgHooks(w http.ResponseWriter, r *http.Request) {
	h.serveHooks(w, r, ghdata.OrgHooksTarget(chi.URLParam(r, "org")))
}

// serveHooks is the shared flow. The two routes differ only in the target they
// name: the request shape, the rebuild, the key, and the invalidation are
// identical, because GitHub answers both with the same hook object.
func (h *handlers) serveHooks(w http.ResponseWriter, r *http.Request, target ghdata.HooksTarget) {
	token := bearerToken(r)
	if token == "" {
		// requireAuth already rejects these; belt and braces so a row can
		// never be stored (or served) under an empty credential key.
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
	if doc, ok, err := h.store.GetCachedHooks(r.Context(), fp, target, perPage, page, now); err != nil {
		slog.Warn("hooks cache read failed", "scope", target.Scope, "owner", target.Owner, "repo", target.Repo, "error", err)
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

	doc, absorbed := absorbHooks(resp.StatusCode, body)
	if overflow || !absorbed {
		// 403 (the caller is not an admin), 404, 5xx, and any shape the model
		// cannot hold: relayed verbatim, never stored. A 403 is deliberately
		// NOT a cached verdict -- a permission grant is exactly the kind of
		// thing that changes without any event reaching the mirror.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedHooks(r.Context(), fp, target, perPage, page, doc, now, hooksCacheTTL); err != nil {
		slog.Warn("hooks cache write failed", "scope", target.Scope, "owner", target.Owner, "repo", target.Repo, "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// writeRepoHooks / writeOrgHooks forward a hook WRITE (recorded as a write,
// like every proxied mutation) after dropping that target's cached listings
// ACROSS EVERY CREDENTIAL -- a hook one caller creates changes the answer every
// caller gets, so a per-credential flush would leave the others reconciling
// against a listing that no longer exists and creating duplicates.
//
// The flush runs BEFORE forwarding, on the Code Quality route's reasoning: a
// failed write that dropped the rows costs one miss, while a successful write
// whose flush was skipped serves a wrong answer for the whole TTL.
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

// hookJSON is one trimmed hook. GitHub's API self-links (url, test_url,
// ping_url, deliveries_url) are dropped; `config.url` is KEPT and is a pinned
// exception to the no-URL rule, on grounds the other exceptions do not need:
// it is not a link into GitHub's API but the hook's own destination, the field
// that says WHICH hook this is. A listing without it does not answer the
// question the endpoint exists for, so trimming it would not be a trimmed
// answer but a broken one.
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

// hookConfigJSON preserves PRESENCE exactly, the PR-files route's stance for
// optional fields: GitHub omits `secret` entirely when no secret is set and
// sends a fixed mask when one is, so the key's presence is itself the answer
// to "is a secret configured" and must not be invented or dropped.
// `insecure_ssl` is documented as either a string or a number, so it rides as
// raw JSON rather than being coerced into one of them.
type hookConfigJSON struct {
	URL         string          `json:"url,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	InsecureSSL json.RawMessage `json:"insecure_ssl,omitempty"`
	Secret      *string         `json:"secret,omitempty"`
}

// hookLastResponseJSON is the delivery outcome GitHub attaches to a repo hook.
// It moves with every delivery and no webhook names the change, so within the
// TTL it is honestly stale -- but omitting a key a consumer branches on would
// be worse than serving it a few minutes old.
type hookLastResponseJSON struct {
	Code    *int64  `json:"code"`
	Status  string  `json:"status"`
	Message *string `json:"message"`
}

// absorbHooks parses a 200 hooks listing into the trimmed document, rendered
// once here so hit and miss serve identical bytes. The body must be an ARRAY
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
	var raw []struct {
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
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return "", false
	}
	out := make([]hookJSON, 0, len(raw))
	for _, hook := range raw {
		if hook.ID <= 0 || hook.Name == "" {
			return "", false
		}
		item := hookJSON{
			ID: hook.ID, Type: hook.Type, Name: hook.Name, Active: hook.Active,
			Events: hook.Events, CreatedAt: hook.CreatedAt, UpdatedAt: hook.UpdatedAt,
		}
		if item.Events == nil {
			item.Events = []string{}
		}
		if hook.Config != nil {
			item.Config = hookConfigJSON{
				URL: hook.Config.URL, ContentType: hook.Config.ContentType,
				InsecureSSL: hook.Config.InsecureSSL, Secret: hook.Config.Secret,
			}
		}
		if hook.LastResponse != nil {
			item.LastResponse = &hookLastResponseJSON{
				Code: hook.LastResponse.Code, Status: hook.LastResponse.Status,
				Message: hook.LastResponse.Message,
			}
		}
		out = append(out, item)
	}
	rendered, err := marshalTrimmed(out)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
