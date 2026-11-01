// Cached org custom property values: GET /orgs/{org}/properties/values.

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

// Cached org custom property values: GET /orgs/{org}/properties/values.
// The answer lists the org repos this token can see, so a row keys the
// bearer's fingerprint and is never shared.

func (h *handlers) cachedOrgPropertyValues(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		h.passthrough(w, r, PassIdentity)
		return
	}
	if !acceptsDefaultJSON(r) {
		h.passthrough(w, r, PassAccept)
		return
	}
	// Paging only; repository_query and any other param pass through.
	perPage, page, ok := parseOrgRunnersShape(r.URL.Query())
	if !ok {
		h.passthrough(w, r, PassQuery)
		return
	}
	org := ghdata.NormalizeRepoKey(chi.URLParam(r, "org"))
	fp := ghclient.Fingerprint(token)

	now := time.Now()
	if a, ok, err := h.store.GetCachedOrgPropertyValues(r.Context(), fp, org, perPage, page, now); err != nil {
		slog.Warn("org property values cache read failed", "org", org, "error", err)
	} else if ok {
		h.reqlog.observeStatus(r, DispHit, a.Status)
		writeRebuilt(w, a.Status, []byte(a.Doc), true)
		return
	}

	resp, body, overflow, err := h.fetchUpstream(r, nil)
	if err != nil {
		h.upstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	a, ttl, absorbed := absorbOrgPropertyValues(resp, body)
	if overflow || !absorbed {
		h.replayUnstored(w, r, resp, body)
		return
	}
	if err := h.store.PutCachedOrgPropertyValues(r.Context(), fp, org, perPage, page, a, now, ttl); err != nil {
		slog.Warn("org property values cache write failed", "org", org, "error", err)
	}
	h.reqlog.observeStatus(r, DispMiss, resp.StatusCode)
	writeRebuilt(w, a.Status, []byte(a.Doc), false)
}

func absorbOrgPropertyValues(resp *http.Response, body []byte) (ghdata.OrgPropertyValuesAnswer, time.Duration, bool) {
	switch {
	case resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusForbidden && !upstreamRateLimited(resp):
		doc, err := marshalTrimmed(notFoundJSON{Message: upstreamErrorMessage(body), Status: strconv.Itoa(resp.StatusCode)})
		if err != nil {
			return ghdata.OrgPropertyValuesAnswer{}, 0, false
		}
		return ghdata.OrgPropertyValuesAnswer{Status: resp.StatusCode, Doc: string(doc)}, ghdata.OrgPropertyValuesVerdictTTL, true
	case resp.StatusCode != http.StatusOK:
		return ghdata.OrgPropertyValuesAnswer{}, 0, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return ghdata.OrgPropertyValuesAnswer{}, 0, false
	}
	var entries []ghdata.OrgPropertyValuesEntry
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return ghdata.OrgPropertyValuesAnswer{}, 0, false
	}
	for i, e := range entries {
		if e.RepositoryID <= 0 || e.RepositoryFullName == "" {
			return ghdata.OrgPropertyValuesAnswer{}, 0, false
		}
		if e.Properties == nil {
			entries[i].Properties = []ghdata.OrgPropertyValue{}
		}
		for j, p := range e.Properties {
			if len(p.Value) == 0 {
				entries[i].Properties[j].Value = json.RawMessage("null")
			}
		}
	}
	if entries == nil {
		entries = []ghdata.OrgPropertyValuesEntry{}
	}
	doc, err := marshalTrimmed(entries)
	if err != nil {
		return ghdata.OrgPropertyValuesAnswer{}, 0, false
	}
	return ghdata.OrgPropertyValuesAnswer{Status: http.StatusOK, Doc: string(doc)}, ghdata.OrgPropertyValuesCacheTTL, true
}
