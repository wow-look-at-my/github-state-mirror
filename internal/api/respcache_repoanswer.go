package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The read path every route on repo_answer_cache shares: reveal, the
// caller's own stored refusal, hit, fetch, absorb, rebuild.

// repoAnswerRoute is one request on the shared table.
type repoAnswerRoute struct {
	key      ghdata.RepoAnswerKey
	denyKind string
	resource string // the deny-cache resource key
	// absorb decides what to store from an upstream answer. now is the instant.
	absorb   func(resp *http.Response, body []byte, now time.Time) (ghdata.RepoAnswer, bool)
	refusals []int
	// fetch, when set, is the upstream request in place of the caller's.
	fetch      *http.Request
	noRedirect bool
	writeOK    func(w http.ResponseWriter, doc string, hit bool)
}

func (h *handlers) writeRouteAnswer(w http.ResponseWriter, r *http.Request, route repoAnswerRoute, a ghdata.RepoAnswer, hit bool) {
	if route.writeOK == nil || a.Status != http.StatusOK {
		h.writeRepoAnswer(w, r, a, hit)
		return
	}
	if hit {
		h.reqlog.observeStatus(r, DispHit, a.Status)
	} else {
		h.reqlog.observeStatus(r, DispMiss, a.Status)
	}
	route.writeOK(w, a.Doc, hit)
}

func (h *handlers) serveRepoAnswer(w http.ResponseWriter, r *http.Request, route repoAnswerRoute) {
	key := route.key
	switch outcome, verdict, cached := h.reveal(r, key.Owner, key.Repo, route.denyKind, route.resource); outcome {
	case revealDenied:
		h.serveDenyVerdict(w, r, verdict, cached)
		return
	case revealError:
		h.revealFailed(w, r)
		return
	}
	if v, ok := h.subresourceVerdict(r, route.denyKind, route.resource); ok {
		h.serveDenyVerdict(w, r, v, true)
		return
	}

	now := time.Now()
	if a, ok, err := h.store.GetRepoAnswer(r.Context(), key, now); err != nil {
		slog.Warn("repo answer cache read failed", "owner", key.Owner, "repo", key.Repo, "kind", key.Kind, "ref", key.RefID, "error", err)
	} else if ok {
		h.writeRouteAnswer(w, r, route, a, true)
		return
	}

	fetch := route.fetch
	if fetch == nil {
		fetch = r
	}
	var resp *http.Response
	var body []byte
	var overflow bool
	var err error
	if route.noRedirect {
		resp, body, overflow, err = h.fetchUpstreamNoRedirect(fetch)
	} else {
		resp, body, overflow, err = h.fetchUpstream(fetch, nil)
	}
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	if v, ok := h.recordSubresourceVerdict(r, key.Owner, key.Repo, route.denyKind, route.resource, resp, body, route.refusals...); ok {
		h.serveDenyVerdict(w, r, v, false)
		return
	}
	if overflow {
		h.replayUnstored(w, r, resp, body)
		return
	}
	a, ok := route.absorb(resp, body, now)
	if !ok || !a.ExpiresAt.After(now) {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutRepoAnswer(r.Context(), key, a, now); err != nil {
		slog.Warn("repo answer cache write failed", "owner", key.Owner, "repo", key.Repo, "kind", key.Kind, "ref", key.RefID, "error", err)
	}
	h.refreshGrantOn2xx(r, key.Owner, key.Repo, resp.StatusCode)
	h.writeRouteAnswer(w, r, route, a, false)
}

// redirectJSON is how a stored redirect holds its Location.
type redirectJSON struct {
	Location string `json:"location"`
}

// writeRepoAnswer serves a stored answer. A redirect carries its Location
// and no body; anything else is a rebuilt JSON document.
func (h *handlers) writeRepoAnswer(w http.ResponseWriter, r *http.Request, a ghdata.RepoAnswer, hit bool) {
	if hit {
		h.reqlog.observeStatus(r, DispHit, a.Status)
	} else {
		h.reqlog.observeStatus(r, DispMiss, a.Status)
	}
	if a.Status != http.StatusFound {
		writeRebuilt(w, a.Status, []byte(a.Doc), hit)
		return
	}
	var redirect redirectJSON
	_ = json.Unmarshal([]byte(a.Doc), &redirect)
	w.Header().Set("Location", redirect.Location)
	if hit {
		w.Header().Set(cacheHeader, "hit")
	} else {
		w.Header().Set(cacheHeader, "miss")
	}
	w.WriteHeader(http.StatusFound)
}

// verdictAnswer stores an authoritative error answer in the trimmed shape the
// other verdict routes serve.
func verdictAnswer(status int, body []byte, expires time.Time) (ghdata.RepoAnswer, bool) {
	doc, err := marshalTrimmed(notFoundJSON{Message: upstreamErrorMessage(body), Status: strconv.Itoa(status)})
	if err != nil {
		return ghdata.RepoAnswer{}, false
	}
	return ghdata.RepoAnswer{Status: status, Doc: string(doc), ExpiresAt: expires}, true
}
