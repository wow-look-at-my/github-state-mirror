package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The reveal layer: WHO may read a piece of global truth.
//
// The truth store is global (one row per resource; webhooks and any caller's
// fetches all feed it), so serving cached state must be gated per caller.
// GitHub itself is the permission oracle -- the mirror never invents an
// authorization model, it only caches GitHub's own answers:
//
//   1. PUBLIC fast path: the repo's webhook/REST-learned visibility is
//      'public' -> any authenticated principal may read its cached state. A
//      repo whose visibility is unknown ('', e.g. seeded only by the
//      identity-locked GraphQL fetch, which cannot carry visibility) is
//      treated as PRIVATE -- fail closed.
//   2. GRANT: the principal holds an unexpired access_grants row for the
//      repo, earned from GitHub's own answers to that principal's requests
//      (an org list-sync with their token, or an earlier probe).
//   3. DENY VERDICT: GitHub authoritatively told this principal "no" (404 /
//      non-rate-limit 403) for this exact resource within the deny TTL ->
//      serve that answer again without asking GitHub.
//   4. PROBE: otherwise ask GitHub: GET /repos/{owner}/{repo} with the
//      caller's own token (buildhost's canAccessRepo pattern). 200 proves
//      access -> absorb the (canonical, visibility-carrying) repository
//      object into truth, record a grant, proceed. 404/authoritative-403 ->
//      record a deny verdict, relay the answer. Transient failures (5xx,
//      429, rate-limited 403, network) are relayed but NEVER cached -- a
//      hiccup must not pin a caller out (or in).
//
// The probe costs one upstream call on a principal's FIRST touch of a
// non-public repo (per grant TTL); it also heals unknown-visibility rows,
// since the probe response carries visibility for everyone's benefit.

// Deny-verdict resource kinds (deny_cache.resource_kind).
const (
	denyKindContents  = "contents"
	denyKindGitCommit = "git_commit"
	denyKindRepoPulls = "repo_pulls"
	denyKindPull      = "pull"
)

// revealOutcome is the reveal decision for one request.
type revealOutcome int

const (
	// revealAllowed: serve/absorb cached state as usual.
	revealAllowed revealOutcome = iota
	// revealDenied: GitHub said no (now or recently); serve the verdict.
	revealDenied
	// revealError: could not decide (transient probe/store failure); the
	// request fails 502 without caching anything.
	revealError
)

// reveal decides whether the caller may read cached state for a repo,
// probing GitHub when it has no answer on file. verdict is set when the
// outcome is revealDenied; cachedVerdict reports whether it came from the
// deny cache (a hit) rather than a fresh probe (a miss).
func (h *handlers) reveal(r *http.Request, owner, repo, kind, resourceKey string) (outcome revealOutcome, verdict ghdata.DenyVerdict, cachedVerdict bool) {
	ctx := r.Context()
	principal := actor.FromContext(ctx)
	now := time.Now()

	// 1. Public fast path (case-insensitive: truth rows carry canonical
	// casing, the URL may not). Unknown visibility falls through -- private
	// until proven otherwise.
	if repoRow, err := h.store.GetRepoInsensitive(ctx, owner, repo); err == nil {
		if repoRow.Visibility == ghdata.VisibilityPublic {
			return revealAllowed, ghdata.DenyVerdict{}, false
		}
	}

	// 2. Grant.
	if principal != "" {
		ok, err := h.store.HasGrant(ctx, principal, owner, repo, now)
		if err != nil {
			slog.Warn("reveal: grant lookup failed", "principal", actor.Short(principal), "repo", owner+"/"+repo, "error", err)
			return revealError, ghdata.DenyVerdict{}, false
		}
		if ok {
			return revealAllowed, ghdata.DenyVerdict{}, false
		}

		// 3. Cached deny verdict for this exact resource.
		v, ok, err := h.store.GetDenyVerdict(ctx, principal, kind, resourceKey, now)
		if err != nil {
			slog.Warn("reveal: deny lookup failed", "principal", actor.Short(principal), "repo", owner+"/"+repo, "error", err)
			return revealError, ghdata.DenyVerdict{}, false
		}
		if ok {
			return revealDenied, v, true
		}
	}

	// 4. Probe GitHub with the caller's own token.
	return h.probeRepoAccess(r, principal, owner, repo, kind, resourceKey)
}

// probeRepoAccess asks GitHub whether the caller can see the repo
// (GET /repos/{owner}/{repo} with their token) and records the answer.
func (h *handlers) probeRepoAccess(r *http.Request, principal, owner, repo, kind, resourceKey string) (revealOutcome, ghdata.DenyVerdict, bool) {
	ctx := r.Context()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.gh.BaseURL()+"/repos/"+owner+"/"+repo, nil)
	if err != nil {
		return revealError, ghdata.DenyVerdict{}, false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := h.upstream.Do(req)
	if err != nil {
		slog.Warn("reveal probe failed", "repo", owner+"/"+repo, "error", err)
		return revealError, ghdata.DenyVerdict{}, false
	}
	defer resp.Body.Close()
	// Passively record the X-RateLimit-* headers the probe response carries.
	h.meter.Observe(callerLabel(r), resp)
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAbsorbBodyBytes))
	if err != nil {
		return revealError, ghdata.DenyVerdict{}, false
	}

	now := time.Now()
	switch {
	case resp.StatusCode == http.StatusOK:
		// GitHub's proof of access. Absorb the repository object (canonical
		// casing, visibility -- global truth learns the repo exists and
		// whether it is public) and grant the principal.
		if repoRow, ok := webhook.ParseRepositoryObject(body); ok {
			if err := h.store.UpsertRepo(ctx, repoRow); err != nil {
				slog.Warn("reveal probe: absorb repo failed", "repo", owner+"/"+repo, "error", err)
			}
		}
		if principal != "" {
			if err := h.store.RecordGrant(ctx, principal, owner, repo, ghdata.GrantSourceProbe, now); err != nil {
				slog.Warn("reveal probe: record grant failed", "principal", actor.Short(principal), "repo", owner+"/"+repo, "error", err)
				return revealError, ghdata.DenyVerdict{}, false
			}
		}
		return revealAllowed, ghdata.DenyVerdict{}, false

	case resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusForbidden && !upstreamRateLimited(resp):
		// Authoritative "no". Their truth: relay it, and remember it briefly
		// so a poll doesn't hammer GitHub. (404 is deliberately keyed to the
		// exact resource, not the repo: GitHub's 404 cannot be told apart
		// from "resource missing inside a repo you CAN see".)
		v := ghdata.DenyVerdict{Status: resp.StatusCode, Message: upstreamErrorMessage(body)}
		if principal != "" {
			if err := h.store.RecordDenyVerdict(ctx, principal, kind, resourceKey, owner, repo, v.Status, v.Message, now); err != nil {
				slog.Warn("reveal probe: record deny failed", "principal", actor.Short(principal), "repo", owner+"/"+repo, "error", err)
			}
			if resp.StatusCode == http.StatusForbidden {
				// A 403 is unambiguous about repo access; a stale grant (if
				// any survived) must go.
				if err := h.store.RevokeGrant(ctx, principal, owner, repo); err != nil {
					slog.Warn("reveal probe: revoke grant failed", "principal", actor.Short(principal), "repo", owner+"/"+repo, "error", err)
				}
			}
		}
		return revealDenied, v, false

	default:
		// Transient (5xx, 429, rate-limited 403): never cached as a deny.
		slog.Warn("reveal probe: transient upstream answer", "repo", owner+"/"+repo, "status", resp.StatusCode)
		return revealError, ghdata.DenyVerdict{}, false
	}
}

// serveDenyVerdict writes a deny verdict as a trimmed GitHub-style error body
// and records the request: a cached verdict is a hit (answered from state), a
// fresh probe answer is a miss (asked GitHub, absorbed the verdict).
func (h *handlers) serveDenyVerdict(w http.ResponseWriter, r *http.Request, v ghdata.DenyVerdict, cached bool) {
	body, err := marshalTrimmed(notFoundJSON{Message: v.Message, Status: strconv.Itoa(v.Status)})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if cached {
		h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispHit, v.Status)
	} else {
		h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispMiss, v.Status)
	}
	writeRebuilt(w, v.Status, body, cached)
}

// revealFailed reports a reveal-layer transient failure (probe/store error):
// the mirror cannot decide access right now, so the request fails without
// caching anything. 502 mirrors the cached routes' upstream-error handling.
func (h *handlers) revealFailed(w http.ResponseWriter, r *http.Request) {
	h.reqlog.recordStatus(callerLabel(r), r.Method, r.URL.Path, DispError, http.StatusBadGateway)
	http.Error(w, "bad gateway: could not verify repository access with GitHub", http.StatusBadGateway)
}

// upstreamRateLimited reports whether a 4xx is GitHub rate limiting rather
// than a permissions answer (mirrors ghclient.looksRateLimited).
func upstreamRateLimited(resp *http.Response) bool {
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	return resp.Header.Get("X-RateLimit-Remaining") == "0"
}

// upstreamErrorMessage extracts GitHub's error message from a 4xx body.
func upstreamErrorMessage(body []byte) string {
	msg := struct {
		Message string `json:"message"`
	}{}
	_ = json.Unmarshal(body, &msg)
	if msg.Message == "" {
		return "Not Found"
	}
	return msg.Message
}
