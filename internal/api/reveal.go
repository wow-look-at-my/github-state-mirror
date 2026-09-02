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

// The reveal layer: WHO may read a piece of global truth. GitHub is the
// permission oracle -- public repo, a live grant, a cached deny verdict, or a
// live probe with the caller's own token decide it, in that order.
// see docs/reveal-layer.md

// Deny-verdict resource kinds (deny_cache.resource_kind).
const (
	denyKindContents       = "contents"
	denyKindGitCommit      = "git_commit"
	denyKindRepoPulls      = "repo_pulls"
	denyKindPull           = "pull"
	denyKindRepoCommits    = "repo_commits"
	denyKindCompare        = "compare"
	denyKindCommitStatus   = "commit_status"
	denyKindCheckRuns      = "check_runs"
	denyKindPullFiles      = "pull_files"    // GET /repos/{owner}/{repo}/pulls/{number}/files
	denyKindBranches       = "branches"      // GET /repos/{owner}/{repo}/branches
	denyKindRepo           = "repo"          // GET /repos/{owner}/{repo}
	denyKindStatusesList   = "statuses_list" // the raw statuses LIST (both path spellings)
	denyKindWorkflowRuns   = "workflow_runs" // GET /repos/{owner}/{repo}/actions/runs?head_sha=
	denyKindGitRef         = "git_ref"       // GET /repos/{owner}/{repo}/git/ref/{ref}
	denyKindRunJobs        = "run_jobs"      // GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs
	denyKindWorkflowJob    = "workflow_job"  // GET /repos/{owner}/{repo}/actions/jobs/{job_id}
	denyKindCodeQuality    = "code_quality"  // GET /repos/{owner}/{repo}/code-quality/setup
	denyKindLabel          = "label"         // GET /repos/{owner}/{repo}/labels/{name}
	denyKindGitTree        = "git_tree"      // GET /repos/{owner}/{repo}/git/trees/{sha}
	denyKindSingleCheckRun = "check_run"     // GET /repos/{owner}/{repo}/check-runs/{check_run_id}
	denyKindMatchingRefs   = "matching_refs" // GET /repos/{owner}/{repo}/git/matching-refs/heads/*
)

// revealOutcome is the reveal decision for request.
type revealOutcome int

const (
	// revealAllowed: serve/absorb cached state as usual.
	revealAllowed revealOutcome = iota
	// revealDenied: GitHub said no (now or recently); serve the verdict.
	revealDenied
	// revealError: could not decide (transient failure); the request fails without caching anything.
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

	// . Public fast path (case-insensitive: truth rows carry canonical
	// casing, the URL may not). Unknown visibility falls through -- private
	// until proven otherwise.
	if repoRow, err := h.store.GetRepoInsensitive(ctx, owner, repo); err == nil {
		if repoRow.Visibility == ghdata.VisibilityPublic {
			return revealAllowed, ghdata.DenyVerdict{}, false
		}
	}

	// . Grant.
	if principal != "" {
		ok, err := h.store.HasGrant(ctx, principal, owner, repo, now)
		if err != nil {
			slog.Warn("reveal: grant lookup failed", "principal", actor.Short(principal), "repo", owner+"/"+repo, "error", err, principalNameAttr(ctx))
			return revealError, ghdata.DenyVerdict{}, false
		}
		if ok {
			return revealAllowed, ghdata.DenyVerdict{}, false
		}

		// . Cached deny verdict for this exact resource.
		v, ok, err := h.store.GetDenyVerdict(ctx, principal, kind, resourceKey, now)
		if err != nil {
			slog.Warn("reveal: deny lookup failed", "principal", actor.Short(principal), "repo", owner+"/"+repo, "error", err, principalNameAttr(ctx))
			return revealError, ghdata.DenyVerdict{}, false
		}
		if ok {
			return revealDenied, v, true
		}
	}

	// . Probe GitHub with the caller's own token.
	return h.probeRepoAccess(r, principal, owner, repo, kind, resourceKey)
}

// probeRepoAccess asks GitHub whether the caller can see the repo
// (GET /repos/{owner}/{repo} with their token) and records the answer.
func (h *handlers) probeRepoAccess(r *http.Request, principal, owner, repo, kind, resourceKey string) (revealOutcome, ghdata.DenyVerdict, bool) {
	ctx := r.Context()
	// The transport charts this as a real mirror->GitHub exchange under dispProbe, failures included.
	req, err := http.NewRequestWithContext(withUpstreamDisposition(ctx, dispProbe),
		http.MethodGet, h.gh.BaseURL()+"/repos/"+owner+"/"+repo, nil)
	if err != nil {
		return revealError, ghdata.DenyVerdict{}, false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	who := callerLabel(r)
	resp, err := h.upstream.Do(req)
	if err != nil {
		slog.Warn("reveal probe failed", "repo", owner+"/"+repo, "error", err)
		return revealError, ghdata.DenyVerdict{}, false
	}
	defer resp.Body.Close()
	// Passively record the X-RateLimit-* headers the probe response carries.
	h.meter.Observe(who.Key, who.Name, resp)
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
				slog.Warn("reveal probe: record grant failed", "principal", actor.Short(principal), "repo", owner+"/"+repo, "error", err, principalNameAttr(ctx))
				return revealError, ghdata.DenyVerdict{}, false
			}
		}
		return revealAllowed, ghdata.DenyVerdict{}, false

	case resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusForbidden && !upstreamRateLimited(resp):
		//  is keyed to the exact resource, not the repo: GitHub's can't be told apart from a readable repo's missing resource.
		v := ghdata.DenyVerdict{Status: resp.StatusCode, Message: upstreamErrorMessage(body)}
		if principal != "" {
			if err := h.store.RecordDenyVerdict(ctx, principal, kind, resourceKey, owner, repo, v.Status, v.Message, now); err != nil {
				slog.Warn("reveal probe: record deny failed", "principal", actor.Short(principal), "repo", owner+"/"+repo, "error", err, principalNameAttr(ctx))
			}
			if resp.StatusCode == http.StatusForbidden {
				// A is unambiguous about repo access; a stale grant (if
				// any survived) must go.
				if err := h.store.RevokeGrant(ctx, principal, owner, repo); err != nil {
					slog.Warn("reveal probe: revoke grant failed", "principal", actor.Short(principal), "repo", owner+"/"+repo, "error", err, principalNameAttr(ctx))
				}
			}
		}
		return revealDenied, v, false

	default:
		// Transient (5xx,, rate-limited): never cached as a deny.
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
		h.reqlog.observeStatus(r, DispHit, v.Status)
	} else {
		h.reqlog.observeStatus(r, DispMiss, v.Status)
	}
	writeRebuilt(w, v.Status, body, cached)
}

// revealFailed reports a reveal-layer transient failure (probe/store error):
// the mirror cannot decide access right now, so the request fails without
// caching anything. mirrors the cached routes' upstream-error handling.
func (h *handlers) revealFailed(w http.ResponseWriter, r *http.Request) {
	h.reqlog.observeStatus(r, DispError, http.StatusBadGateway)
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
