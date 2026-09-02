package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// Cached installation-repositories listing (tier of the cache contract): GET /installation/repositories
// see docs/cache/rest-routes.md

const (
	// installationReposTTL is the PRIMARY bound on a row, not a backstop: the mirror sees only its own App's flush events.
	installationReposTTL = 15 * time.Minute

	installationReposDefaultPerPage = 30
	// installationReposMaxCachedPage caps the modeled pages; deeper pagination passes through.
	installationReposMaxCachedPage = 20
)

// cachedInstallationRepos serves page of the caller's own installation
// repository listing.
func (h *handlers) cachedInstallationRepos(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		// Belt and braces: a row must never be stored or served under an empty key.
		h.passthrough(w, r, PassIdentity)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	perPage, page, ok := parseInstallationReposShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}

	fp := ghclient.Fingerprint(token)
	now := time.Now()
	if doc, ok, err := h.store.GetCachedInstallationRepos(r.Context(), fp, perPage, page, now); err != nil {
		slog.Warn("installation repos cache read failed", "error", err)
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

	doc, absorbed := absorbInstallationRepos(resp.StatusCode, body)
	if overflow || !absorbed {
		// / and any unmodeled shape: relayed verbatim, never stored.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedInstallationRepos(r.Context(), fp, perPage, page, doc, now, installationReposTTL); err != nil {
		slog.Warn("installation repos cache write failed", "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// parseInstallationReposShape reports the paging shape and whether the cache
// models it. The endpoint takes only per_page/page.
func parseInstallationReposShape(q url.Values) (perPage, page int64, ok bool) {
	perPage, page = installationReposDefaultPerPage, 1
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
			if n < 1 || n > installationReposMaxCachedPage {
				return 0, 0, false
			}
			page = n
		default:
			return 0, 0, false
		}
	}
	return perPage, page, true
}

// installationReposJSON is the trimmed rebuild. total_count is GitHub's TOTAL
// matching count, not the page length.
type installationReposJSON struct {
	TotalCount          int64                  `json:"total_count"`
	RepositorySelection *string                `json:"repository_selection"`
	Repositories        []installationRepoJSON `json:"repositories"`
}

// installationRepoJSON is trimmed repository entry: the identity and
// state fields, with the dozens of *_url templates GitHub attaches dropped.
// `visibility` is nullable-but-always-keyed rather than defaulted -- an
// answer that did not carry it must not read as "public".
type installationRepoJSON struct {
	ID            int64                 `json:"id"`
	NodeID        string                `json:"node_id"`
	Name          string                `json:"name"`
	FullName      string                `json:"full_name"`
	Owner         installationOwnerJSON `json:"owner"`
	Private       bool                  `json:"private"`
	Visibility    *string               `json:"visibility"`
	DefaultBranch string                `json:"default_branch"`
	Fork          bool                  `json:"fork"`
	Archived      bool                  `json:"archived"`
	Disabled      bool                  `json:"disabled"`
}

type installationOwnerJSON struct {
	Login string `json:"login"`
	Type  string `json:"type,omitempty"`
}

// absorbInstallationRepos parses a into the trimmed document, rendered
//
//	here so hit and miss serve identical bytes. total_count and the
//
// repositories array must both be PRESENT, and every entry must carry a
// positive id and a full name -- an answer the model cannot hold is relayed
// instead. An empty page (past the end) is a valid cacheable answer.
func absorbInstallationRepos(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false
	}
	var raw struct {
		TotalCount          *int64  `json:"total_count"`
		RepositorySelection *string `json:"repository_selection"`
		Repositories        *[]struct {
			ID     int64  `json:"id"`
			NodeID string `json:"node_id"`
			Name   string `json:"name"`
			Owner  *struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"owner"`
			FullName      string  `json:"full_name"`
			Private       bool    `json:"private"`
			Visibility    *string `json:"visibility"`
			DefaultBranch string  `json:"default_branch"`
			Fork          bool    `json:"fork"`
			Archived      bool    `json:"archived"`
			Disabled      bool    `json:"disabled"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.TotalCount == nil || raw.Repositories == nil {
		return "", false
	}
	out := installationReposJSON{
		TotalCount:          *raw.TotalCount,
		RepositorySelection: raw.RepositorySelection,
		Repositories:        []installationRepoJSON{},
	}
	for _, repo := range *raw.Repositories {
		if repo.ID <= 0 || repo.FullName == "" {
			return "", false
		}
		item := installationRepoJSON{
			ID: repo.ID, NodeID: repo.NodeID, Name: repo.Name, FullName: repo.FullName,
			Private: repo.Private, Visibility: repo.Visibility,
			DefaultBranch: repo.DefaultBranch, Fork: repo.Fork,
			Archived: repo.Archived, Disabled: repo.Disabled,
		}
		if repo.Owner != nil {
			item.Owner = installationOwnerJSON{Login: repo.Owner.Login, Type: repo.Owner.Type}
		}
		out.Repositories = append(out.Repositories, item)
	}
	rendered, err := marshalTrimmed(out)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
