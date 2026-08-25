package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// GET /repos/{owner}/{repo}/code-quality/setup, tier 2 of the cache contract.
// see docs/cache/rest-routes.md

// The primary bound, not a backstop: see docs/cache/rest-routes.md
const codeQualitySetupTTL = time.Hour

// cachedCodeQualitySetup serves a repo's Code Quality configuration.
func (h *handlers) cachedCodeQualitySetup(w http.ResponseWriter, r *http.Request) {
	owner, repo := chi.URLParam(r, "owner"), chi.URLParam(r, "repo")

	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	if len(r.URL.Query()) > 0 {
		// The endpoint takes no parameters, so any is unmodeled by definition.
		h.passthrough(w, r, PassQuery)
		return
	}

	resourceKey := ghdata.NormalizeRepoKey(owner) + "/" + ghdata.NormalizeRepoKey(repo) + "/code-quality/setup"
	switch outcome, verdict, cached := h.reveal(r, owner, repo, denyKindCodeQuality, resourceKey); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}

	now := time.Now()
	if doc, ok, err := h.store.GetCachedCodeQualitySetup(r.Context(), owner, repo, now); err != nil {
		slog.Warn("code quality setup cache read failed", "owner", owner, "repo", repo, "error", err)
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

	doc, absorbed := absorbCodeQualitySetup(resp.StatusCode, body)
	if overflow || !absorbed {
		// Non-200s relay unstored; see docs/cache/rest-routes.md.
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedCodeQualitySetup(r.Context(), owner, repo, doc, now, codeQualitySetupTTL); err != nil {
		slog.Warn("code quality setup cache write failed", "owner", owner, "repo", repo, "error", err)
	}
	h.refreshGrantOn2xx(r, owner, repo, resp.StatusCode)
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, http.StatusOK, []byte(doc), false)
}

// Flushes before forwarding so the caller can't read back its own stale
// config. see docs/cache/rest-routes.md
func (h *handlers) patchCodeQualitySetup(w http.ResponseWriter, r *http.Request) {
	owner, repo := chi.URLParam(r, "owner"), chi.URLParam(r, "repo")
	if err := h.store.InvalidateCodeQualitySetup(r.Context(), owner, repo); err != nil {
		slog.Warn("code quality setup flush on write failed", "owner", owner, "repo", repo, "error", err)
	}
	h.ghProxy.ServeHTTP(w, r)
}

// codeQualitySetupJSON is the trimmed rebuild -- which here is the whole
// documented record, because GitHub's `code-quality-setup` schema carries no
// url field to drop. Every nullable field keeps its key (a consumer branches
// on null vs a value), and `languages` is always emitted as an array.
type codeQualitySetupJSON struct {
	State            string   `json:"state"`
	Languages        []string `json:"languages"`
	RunnerType       *string  `json:"runner_type"`
	RunnerLabel      *string  `json:"runner_label"`
	UpdatedAt        *string  `json:"updated_at"`
	Schedule         *string  `json:"schedule"`
	AIFindingsOption *string  `json:"ai_findings_option"`
}

// absorbCodeQualitySetup parses a 200 into the trimmed document, rendered once
// so hit and miss serve identical bytes. `state` is the one field the model
// requires: an answer without it is not the record this route holds.
func absorbCodeQualitySetup(status int, body []byte) (string, bool) {
	if status != http.StatusOK {
		return "", false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false
	}
	var raw struct {
		State            string   `json:"state"`
		Languages        []string `json:"languages"`
		RunnerType       *string  `json:"runner_type"`
		RunnerLabel      *string  `json:"runner_label"`
		UpdatedAt        *string  `json:"updated_at"`
		Schedule         *string  `json:"schedule"`
		AIFindingsOption *string  `json:"ai_findings_option"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw.State == "" {
		return "", false
	}
	out := codeQualitySetupJSON{
		State: raw.State, Languages: raw.Languages,
		RunnerType: raw.RunnerType, RunnerLabel: raw.RunnerLabel,
		UpdatedAt: raw.UpdatedAt, Schedule: raw.Schedule,
		AIFindingsOption: raw.AIFindingsOption,
	}
	if out.Languages == nil {
		out.Languages = []string{}
	}
	rendered, err := marshalTrimmed(out)
	if err != nil {
		return "", false
	}
	return string(rendered), true
}
