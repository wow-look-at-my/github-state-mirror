package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

func (h *handlers) pullRequest(w http.ResponseWriter, r *http.Request) {
	// GitHub varies some REST representations by media type. Cache only the
	// default JSON shape; media variants and query variants pass through.
	if r.URL.RawQuery != "" || hasConditionalHeaders(r) || !cacheablePullRequestAccept(r.Header.Get("Accept")) {
		h.ghProxy.ServeHTTP(w, r)
		return
	}

	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	number, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
	if owner == "" || repo == "" || err != nil || number <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	key := syncpkg.PullRequestKey(owner, repo, number)
	outcome, ensureErr := h.mgr.EnsureFreshOutcome(r.Context(), freshness.ResourceID{
		Kind: syncpkg.KindPullRequestRaw,
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

	raw, err := h.store.GetRESTResponse(r.Context(), syncpkg.KindPullRequestRaw, key)
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

func (h *handlers) listPullRequests(w http.ResponseWriter, r *http.Request) {
	if hasConditionalHeaders(r) || !cacheablePullRequestAccept(r.Header.Get("Accept")) {
		h.ghProxy.ServeHTTP(w, r)
		return
	}

	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	if owner == "" || repo == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	key := syncpkg.RepoPullListKey(owner, repo, r.Header.Get("Accept"), r.Header.Get("X-GitHub-Api-Version"), r.URL.RawQuery)
	outcome, ensureErr := h.mgr.EnsureFreshOutcome(r.Context(), freshness.ResourceID{
		Kind: syncpkg.KindRepoPullList,
		Key:  key,
	})
	if ensureErr != nil {
		h.ghProxy.ServeHTTP(w, r)
		return
	}

	raw, err := h.store.GetRESTResponse(r.Context(), syncpkg.KindRepoPullList, key)
	if err != nil {
		h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, DispError)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	disp := DispHit
	if outcome == freshness.OutcomeMiss {
		disp = DispMiss
	}
	h.reqlog.record(callerLabel(r), r.Method, r.URL.Path, disp)
	writeCachedRESTResponse(w, raw)
}

func writeCachedRESTResponse(w http.ResponseWriter, raw ghdata.RESTResponse) {
	if raw.ContentType.Valid && raw.ContentType.String != "" {
		w.Header().Set("Content-Type", raw.ContentType.String)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(int(raw.StatusCode))
	_, _ = w.Write(raw.Body)
}

func cacheablePullRequestAccept(accept string) bool {
	if accept == "" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		media := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		switch media {
		case "", "*/*", "application/json", "application/vnd.github+json":
			return true
		}
	}
	return false
}

func hasConditionalHeaders(r *http.Request) bool {
	return r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != ""
}
