// Unlike every other cached route there is NO snapshot table.

package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// This file implements the cached bare-repository read (tier of the cache contract, like respcache.go):
//
//	GET /repos/{owner}/{repo}
//
// Unlike every other cached route there is NO snapshot table. The rebuild
// comes straight from the `repos` TRUTH row -- the same webhook-maintained
// (repository events), fleet-synced, consistency-checked state tier already
// serves. So there is deliberately no per-row TTL either. Truth freshness IS
// the service's core model; a reveal probe additionally re-absorbs the row
// per principal within the <=24h grant TTL. The pinned consumers read only
// fields the row carries: the pr-minder-reconcile hook's getDefaultBranch
// (default_branch) and status-only access checks (buildhost's canAccessRepo
// pattern -- the is the answer).

// cachedRepo serves the bare repository read from the repos truth row,
// fetching and absorbing on an incomplete or missing row.
func (h *handlers) cachedRepo(w http.ResponseWriter, r *http.Request) {
	owner := chi.URLParam(r, "owner")
	repo := chi.URLParam(r, "repo")

	if r.URL.RawQuery != "" || !acceptsDefaultJSON(r) {
		h.passthrough(w, r, shapeReason(r, true))
		return
	}

	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindRepo, ghdata.NormalizeRepoKey(owner)+"/"+ghdata.NormalizeRepoKey(repo)); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	// A private repo's row may already be absorbed by the reveal probe; this still counts as a hit.
	row, err := h.store.GetRepoInsensitive(r.Context(), owner, repo)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		slog.Warn("repo truth read failed", "owner", owner, "repo", repo, "error", err)
	}
	if err == nil && repoRowCompleteWithParent(row) {
		h.serveRepoMeta(w, r, row, true)
		return
	}

	// Miss: fetch from GitHub with the caller's own credentials.
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	if overflow || resp.StatusCode != http.StatusOK {
		// Includes an authoritative, relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	repoRow, ok := webhook.ParseRepositoryObject(body)
	if !ok {
		h.replayUnstored(w, r, resp, body)
		return
	}
	// Absorb into truth either way (the COALESCE upsert can only add
	// knowledge)...
	if err := h.store.UpsertRepo(r.Context(), repoRow); err != nil {
		slog.Warn("repo truth absorb failed", "owner", owner, "repo", repo, "error", err)
	}
	// A partial answer passes through verbatim rather than rendering a hole.
	if !repoRowCompleteWithParent(repoRow) {
		h.replayUnstored(w, r, resp, body)
		return
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	h.serveRepoMeta(w, r, repoRow, false)
}

// repoRowComplete reports whether the row has every field a listing item emits.
func repoRowComplete(row dbgen.Repo) bool {
	return row.Visibility != "" &&
		row.DefaultBranch.Valid && row.DefaultBranch.String != "" &&
		row.NameWithOwner != "" &&
		row.IsFork.Valid
}

// repoRowCompleteWithParent reports whether the row has every field the bare
// read emits, a fork's parent among them.
func repoRowCompleteWithParent(row dbgen.Repo) bool {
	return repoRowComplete(row) &&
		(row.IsFork.Int64 == 0 || (row.ParentFullName.String != "" && row.ParentDefaultBranch.String != ""))
}

// serveRepoMeta rebuilds and writes the trimmed repository body. Hit (truth
// row) and miss (just-parsed response row) both go through here, so the
// served shape is identical regardless of cache state.
func (h *handlers) serveRepoMeta(w http.ResponseWriter, r *http.Request, row dbgen.Repo, hit bool) {
	meta := renderRepoMeta(row)
	if meta.Fork {
		meta.Parent = &repoMetaParentJSON{FullName: row.ParentFullName.String, DefaultBranch: row.ParentDefaultBranch.String}
	}
	body, err := marshalTrimmed(meta)
	if err != nil {
		slog.Warn("repo meta render failed", "path", r.URL.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hit {
		h.reqlog.observe(r, DispHit)
	}
	writeRebuilt(w, http.StatusOK, body, hit)
}

// repoMetaOwnerJSON is the trimmed owner object ({"login": ...}).
type repoMetaOwnerJSON struct {
	Login string `json:"login"`
}

// repoMetaParentJSON is a fork's trimmed parent: the repository and the branch
// a fork is measured against.
type repoMetaParentJSON struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

// repoMetaJSON is the trimmed rebuild of a bare repository read: exactly the
// state fields truth holds and consumers read. private is derived (GitHub
// REST reports private=true for internal repos too); url/pushed_at/id and
// every *_url field stay dropped, the parent's clone_url included.
type repoMetaJSON struct {
	Name          string              `json:"name"`
	FullName      string              `json:"full_name"`
	Owner         repoMetaOwnerJSON   `json:"owner"`
	Private       bool                `json:"private"`
	Visibility    string              `json:"visibility"`
	DefaultBranch string              `json:"default_branch"`
	Archived      bool                `json:"archived"`
	Disabled      bool                `json:"disabled"`
	Fork          bool                `json:"fork"`
	Parent        *repoMetaParentJSON `json:"parent,omitempty"`
}

// renderRepoMeta rebuilds the trimmed body from a (complete) repos row.
func renderRepoMeta(row dbgen.Repo) repoMetaJSON {
	login := row.Owner
	if row.OwnerLogin.Valid && row.OwnerLogin.String != "" {
		login = row.OwnerLogin.String
	}
	out := repoMetaJSON{
		Name:          row.Name,
		FullName:      row.NameWithOwner,
		Owner:         repoMetaOwnerJSON{Login: login},
		Private:       row.Visibility != ghdata.VisibilityPublic,
		Visibility:    row.Visibility,
		DefaultBranch: row.DefaultBranch.String,
		Archived:      row.IsArchived != 0,
		Disabled:      row.IsDisabled != 0,
		Fork:          row.IsFork.Int64 != 0,
	}
	return out
}
