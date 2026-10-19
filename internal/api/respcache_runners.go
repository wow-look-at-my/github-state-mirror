package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The cached runner reads other than the org listing: one org runner, the
// repo listing, and one repo runner.

// cachedOrgRunner serves GET /orgs/{org}/actions/runners/{runner_id}.
func (h *handlers) cachedOrgRunner(w http.ResponseWriter, r *http.Request) {
	h.serveSingleRunner(w, r, ghdata.OrgRunnerScope(chi.URLParam(r, "org")))
}

// cachedRepoRunner serves GET /repos/{owner}/{repo}/actions/runners/{runner_id}.
func (h *handlers) cachedRepoRunner(w http.ResponseWriter, r *http.Request) {
	h.serveSingleRunner(w, r, ghdata.RepoRunnerScope(chi.URLParam(r, "owner"), chi.URLParam(r, "repo")))
}

func (h *handlers) serveSingleRunner(w http.ResponseWriter, r *http.Request, scope string) {
	token := bearerToken(r)
	if token == "" {
		h.passthrough(w, r, PassIdentity)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "runner_id"), 10, 64)
	if err != nil || id <= 0 {
		h.passthrough(w, r, PassPath)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	if len(r.URL.Query()) > 0 {
		h.passthrough(w, r, PassQuery)
		return
	}
	key := ghdata.RunnerAnswerKey{TokenFP: ghclient.Fingerprint(token), Scope: scope, RunnerID: id}
	if h.serveRunnerHit(w, r, key) {
		return
	}

	now := time.Now()
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	status, doc, ttl := resp.StatusCode, string(body), ghdata.OrgRunnersCacheTTL
	switch {
	case overflow:
		h.replayUnstored(w, r, resp, body)
		return
	case resp.StatusCode == http.StatusOK && runnerIDOf(body) == id:
	case resp.StatusCode == http.StatusNotFound:
		trimmed, mErr := marshalTrimmed(notFoundJSON{Message: upstreamErrorMessage(body), Status: "404"})
		if mErr != nil {
			h.replayUnstored(w, r, resp, body)
			return
		}
		doc, ttl = string(trimmed), ghdata.RunnerGoneTTL
	default:
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutRunnerAnswer(r.Context(), key, status, doc, now, ttl); err != nil {
		slog.Warn("runner cache write failed", "scope", scope, "runner", id, "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, status)
	writeRebuilt(w, status, []byte(doc), false)
}

// cachedRepoRunners serves GET /repos/{owner}/{repo}/actions/runners.
func (h *handlers) cachedRepoRunners(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		h.passthrough(w, r, PassIdentity)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	perPage, page, ok := parseOrgRunnersShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	fp := ghclient.Fingerprint(token)
	scope := ghdata.RepoRunnerScope(chi.URLParam(r, "owner"), chi.URLParam(r, "repo"))
	key := ghdata.RunnerAnswerKey{TokenFP: fp, Scope: scope, PerPage: perPage, Page: page}
	if h.serveRunnerHit(w, r, key) {
		return
	}

	now := time.Now()
	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	runners, ok := runnersOfListing(body)
	if overflow || resp.StatusCode != http.StatusOK || !ok {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutRunnerAnswer(r.Context(), key, http.StatusOK, string(body), now, ghdata.OrgRunnersCacheTTL); err != nil {
		slog.Warn("repo runners cache write failed", "scope", scope, "error", err)
	}
	h.absorbListedRunners(r, fp, scope, runners, now)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, body, false)
}

// serveRunnerHit writes a stored answer and reports whether there was one.
func (h *handlers) serveRunnerHit(w http.ResponseWriter, r *http.Request, key ghdata.RunnerAnswerKey) bool {
	status, doc, ok, err := h.store.GetRunnerAnswer(r.Context(), key, time.Now())
	if err != nil {
		slog.Warn("runner cache read failed", "scope", key.Scope, "runner", key.RunnerID, "error", err)
		return false
	}
	if !ok {
		return false
	}
	h.reqlog.observeStatus(r, DispHit, status)
	writeRebuilt(w, status, []byte(doc), true)
	return true
}

// absorbListedRunners stores each runner of a fetched listing as the answer
// of its own single read, for the same credential.
func (h *handlers) absorbListedRunners(r *http.Request, fp, scope string, runners []json.RawMessage, now time.Time) {
	for _, raw := range runners {
		id := runnerIDOf(raw)
		if id <= 0 {
			continue
		}
		key := ghdata.RunnerAnswerKey{TokenFP: fp, Scope: scope, RunnerID: id}
		if err := h.store.PutRunnerAnswer(r.Context(), key, http.StatusOK, string(raw), now, ghdata.OrgRunnersCacheTTL); err != nil {
			slog.Warn("listed runner cache write failed", "scope", scope, "runner", id, "error", err)
			return
		}
	}
}

// runnersOfListing returns the runner objects of a listing body. It reports
// false for a body that is not a listing.
func runnersOfListing(body []byte) ([]json.RawMessage, bool) {
	var raw struct {
		TotalCount *int64             `json:"total_count"`
		Runners    *[]json.RawMessage `json:"runners"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.TotalCount == nil || raw.Runners == nil {
		return nil, false
	}
	return *raw.Runners, true
}

func runnerIDOf(body []byte) int64 {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return 0
	}
	var raw struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return 0
	}
	return raw.ID
}
