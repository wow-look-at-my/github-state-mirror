package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

func (h *handlers) repoContents(w http.ResponseWriter, r *http.Request) {
	// GitHub's contents endpoint varies by media type. Cache only the default
	// JSON file object shape; raw/html/object media variants pass through.
	if hasConditionalHeaders(r) || !cacheablePullRequestAccept(r.Header.Get("Accept")) {
		h.ghProxy.ServeHTTP(w, r)
		return
	}

	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	path := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	if owner == "" || repo == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	key := syncpkg.RepoContentsKey(owner, repo, path, r.URL.RawQuery)
	outcome, ensureErr := h.mgr.EnsureFreshOutcome(r.Context(), freshness.ResourceID{
		Kind: syncpkg.KindRepoContents,
		Key:  key,
	})
	disp := DispHit
	switch {
	case ensureErr != nil:
		disp = DispError
	case outcome == freshness.OutcomeMiss:
		disp = DispMiss
	}
	h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, disp)

	raw, err := h.store.GetRESTResponse(r.Context(), syncpkg.KindRepoContents, key)
	if err != nil {
		if ensureErr != nil && errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeCachedRESTResponse(w, raw)
}
