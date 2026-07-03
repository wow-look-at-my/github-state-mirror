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

// repoInstallationCacheTTL is the TTL backstop on cached
// GET /repos/{o}/{r}/installation answers; installation events flush sooner.
const repoInstallationCacheTTL = 24 * time.Hour

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
	if c, ok, err := h.store.GetCachedRepoInstallation(ctx, actorKey, owner, repo, now); err == nil && ok {
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
	if err := h.store.PutCachedRepoInstallation(ctx, actorKey, c, now, repoInstallationCacheTTL); err != nil {
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
