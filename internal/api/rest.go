package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/freshness"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
)

type handlers struct {
	mgr   *freshness.Manager
	store *ghdata.Store
}

func (h *handlers) getUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := h.mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: syncpkg.KindUser, Key: "self"}); err != nil {
		slog.Warn("ensure fresh user failed", "error", err)
	}

	user, err := h.store.GetFirstUser(ctx)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]interface{}{
		"login":      user.Login,
		"avatar_url": user.AvatarUrl,
		"html_url":   user.Url,
	})
}

func (h *handlers) getUserOrgs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Need user login first.
	if err := h.mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: syncpkg.KindUser, Key: "self"}); err != nil {
		slog.Warn("ensure fresh user failed", "error", err)
	}
	user, err := h.store.GetFirstUser(ctx)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	if err := h.mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: syncpkg.KindUserOrgs, Key: user.Login}); err != nil {
		slog.Warn("ensure fresh user orgs failed", "error", err)
	}

	orgs, err := h.store.ListUserOrgs(ctx, user.Login)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	result := make([]map[string]interface{}, len(orgs))
	for i, o := range orgs {
		result[i] = map[string]interface{}{
			"login":      o.Login,
			"avatar_url": o.AvatarUrl.String,
			"url":        o.Url.String,
		}
	}
	writeJSON(w, result)
}

func (h *handlers) getCompare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	base := chi.URLParam(r, "base")
	head := chi.URLParam(r, "head")

	key := fmt.Sprintf("%s/%s/%s...%s", owner, repo, base, head)
	if err := h.mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: syncpkg.KindCompare, Key: key}); err != nil {
		slog.Warn("ensure fresh compare failed", "error", err)
	}

	comp, err := h.store.GetComparison(ctx, owner, repo, base, head)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ahead_by":  comp.AheadBy,
		"behind_by": comp.BehindBy,
	})
}

func (h *handlers) getPRFiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")
	numberStr := chi.URLParam(r, "number")
	number, err := strconv.ParseInt(numberStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid PR number", http.StatusBadRequest)
		return
	}

	key := fmt.Sprintf("%s/%s/%d", owner, repo, number)
	if err := h.mgr.EnsureFresh(ctx, freshness.ResourceID{Kind: syncpkg.KindPRFiles, Key: key}); err != nil {
		slog.Warn("ensure fresh pr files failed", "error", err)
	}

	files, err := h.store.ListPRFiles(ctx, owner, repo, number)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	result := make([]map[string]interface{}, len(files))
	for i, f := range files {
		result[i] = map[string]interface{}{
			"filename":  f.Path,
			"additions": f.Additions,
			"deletions": f.Deletions,
		}
	}
	writeJSON(w, result)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("json encode failed", "error", err)
	}
}
