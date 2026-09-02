package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/actor"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

const (
	// repoInstallationCacheTTL backstops a cached installation ANSWER; events flush it sooner.
	repoInstallationCacheTTL = 24 * time.Hour

	// installationAbsentTTL is the PRIMARY bound for a "not installed" verdict:
	installationAbsentTTL = 5 * time.Minute
)

// ---- GET /repos/{owner}/{repo}/installation ----

// cachedRepoInstallation caches the App-level repo-installation lookup. Like
// the token-mint route it sits OUTSIDE requireAuth (its bearer is a GitHub
// App JWT, which cannot resolve GET /user): the handler verifies the JWT
// itself and partitions by the verified app id. Unverifiable callers forward
// unchanged, uncached (GitHub answers them itself).
func (h *handlers) cachedRepoInstallation(w http.ResponseWriter, r *http.Request) {
	jwt := bearerToken(r)
	if jwt == "" {
		h.passthrough(w, r, PassIdentity) // the proxy 401s tokenless requests
		return
	}
	ident, err := h.gh.VerifyAppIdentity(r.Context(), jwt)
	if err != nil {
		h.passthrough(w, r, PassIdentity)
		return
	}
	if !acceptsDefaultJSON(r) || r.URL.RawQuery != "" {
		h.passthrough(w, r, shapeReason(r, true))
		return
	}
	actorKey := fmt.Sprintf("app:%d", ident.ID)
	ctx := actor.WithActor(r.Context(), actorKey)
	if ident.Slug != "" {
		ctx = actor.WithName(ctx, ident.Slug)
	}
	// Outside requireAuth, so record identity here for the dashboard's app:<id> -> slug lookup.
	if h.recordIdentity != nil {
		h.recordIdentity(ctx, actorKey, ident.Slug)
	}
	who := callerIdent{Key: actorKey, Name: ident.Slug}
	owner := ghdata.NormalizeRepoKey(chi.URLParam(r, "owner"))
	repo := ghdata.NormalizeRepoKey(chi.URLParam(r, "repo"))

	now := time.Now()
	if c, ok, err := h.store.GetCachedRepoInstallation(ctx, actorKey, owner, repo, now); err == nil && ok {
		h.reqlog.observeAs(r, who, DispHit, 0)
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
	if err := h.store.PutCachedRepoInstallation(ctx, actorKey, c, now, installationTTL(c)); err != nil {
		slog.Warn("repo installation cache write failed", "owner", owner, "repo", repo, "error", err)
	}
	h.reqlog.observeAs(r, who, DispMiss, resp.StatusCode)
	h.serveRepoInstallation(w, c, false)
}

// installationTTL: a real installation gets the long backstop; a verdict gets the short primary bound.
func installationTTL(c ghdata.CachedRepoInstallation) time.Duration {
	if c.Status == http.StatusNotFound {
		return installationAbsentTTL
	}
	return repoInstallationCacheTTL
}

// repoInstallationJSON is the trimmed rebuild: GitHub's installation object
// minus every *_url field and the untracked clutter (permissions, events,
// timestamps). pr-minder reads only.id.
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
	if c.Status == http.StatusNotFound {
		body, err := marshalTrimmed(notFoundJSON{Message: c.Message, Status: "404"})
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeRebuilt(w, http.StatusNotFound, body, hit)
		return
	}
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

// absorbRepoInstallation parses an upstream installation response into either
// a well-formed or the authoritative VERDICT ("not installed here").
// Anything else -- a the JWT earned, a 5xx -- reports false and is
// replayed unstored.
//
// The verdict is cacheable on the contents/compare/git-ref precedent: it is
// GitHub's own authoritative answer, and a fleet sweep re-asks it per account
// forever (forwards in process, every a). What keeps it
// honest is its short TTL, NOT a webhook -- see installationAbsentTTL.
func absorbRepoInstallation(owner, repo string, status int, body []byte) (ghdata.CachedRepoInstallation, bool) {
	if status == http.StatusNotFound {
		return ghdata.CachedRepoInstallation{
			Owner: owner, Repo: repo,
			Status: http.StatusNotFound, Message: upstreamErrorMessage(body),
		}, true
	}
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
		Owner: owner, Repo: repo, Status: http.StatusOK, InstallationID: g.ID,
		RepositorySelection: g.RepositorySelection,
		AppID:               g.AppID, AppSlug: g.AppSlug, TargetType: g.TargetType,
	}
	if g.Account != nil {
		c.AccountLogin, c.AccountType = g.Account.Login, g.Account.Type
	}
	return c, true
}

// ---- GET /orgs/{org}/installation and GET /users/{username}/installation ----

// Owner-level installation lookups reuse the repo route's absorb/rebuild/row
const (
	ownerInstallScopeOrg  = "*org"
	ownerInstallScopeUser = "*user"
)

// cachedOwnerInstallation serves an owner-level installation lookup. scope
// picks which sentinel (and therefore which question) the row answers;
// ownerParam names the chi URL parameter carrying the login.
func (h *handlers) cachedOwnerInstallation(scope, ownerParam string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jwt := bearerToken(r)
		if jwt == "" {
			h.passthrough(w, r, PassIdentity) // the proxy 401s tokenless requests
			return
		}
		ident, err := h.gh.VerifyAppIdentity(r.Context(), jwt)
		if err != nil {
			h.passthrough(w, r, PassIdentity)
			return
		}
		if !acceptsDefaultJSON(r) || r.URL.RawQuery != "" {
			h.passthrough(w, r, shapeReason(r, true))
			return
		}
		actorKey := fmt.Sprintf("app:%d", ident.ID)
		ctx := actor.WithActor(r.Context(), actorKey)
		if ident.Slug != "" {
			ctx = actor.WithName(ctx, ident.Slug)
		}
		if h.recordIdentity != nil {
			h.recordIdentity(ctx, actorKey, ident.Slug)
		}
		who := callerIdent{Key: actorKey, Name: ident.Slug}
		owner := ghdata.NormalizeRepoKey(chi.URLParam(r, ownerParam))

		now := time.Now()
		if c, ok, err := h.store.GetCachedRepoInstallation(ctx, actorKey, owner, scope, now); err == nil && ok {
			h.reqlog.observeAs(r, who, DispHit, 0)
			h.serveRepoInstallation(w, c, true)
			return
		} else if err != nil {
			slog.Warn("owner installation cache read failed", "owner", owner, "scope", scope, "error", err)
		}

		resp, body, overflow, err := h.fetchUpstream(r, nil)
		if err != nil {
			h.upstreamError(w, r, err)
			return
		}
		defer resp.Body.Close()

		c, absorbed := absorbRepoInstallation(owner, scope, resp.StatusCode, body)
		if overflow || !absorbed {
			h.replayUnstored(w, r, resp, body)
			return
		}
		if err := h.store.PutCachedRepoInstallation(ctx, actorKey, c, now, installationTTL(c)); err != nil {
			slog.Warn("owner installation cache write failed", "owner", owner, "scope", scope, "error", err)
		}
		h.reqlog.observeAs(r, who, DispMiss, resp.StatusCode)
		h.serveRepoInstallation(w, c, false)
	}
}
