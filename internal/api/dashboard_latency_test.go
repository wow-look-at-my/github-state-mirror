package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLatency_RecordsWebhookHandlingTime(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	postWebhookJSON(t, router, "label", map[string]any{
		"action":     "created",
		"label":      map[string]any{"name": "bug"},
		"repository": map[string]any{"full_name": "org1/repo1", "name": "repo1", "owner": map[string]any{"login": "org1"}},
	})

	req := httptest.NewRequest("GET", "/api/latency", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body latencyResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, float64(GitHubDeliveryTimeoutMs), body.WebhookTimeoutMs)
	require.NotEmpty(t, body.BoundsMs)
	counts := map[string]int64{}
	for _, k := range body.Kinds {
		counts[k.Kind] = k.Count
		assert.Len(t, k.Buckets, len(body.BoundsMs)+1, "%s: one bucket per edge plus overflow", k.Kind)
	}
	assert.Equal(t, int64(1), counts[KindWebhook], "the delivery's handling time is on the Latency tab")
}

func TestLatency_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)
	req := httptest.NewRequest("GET", "/api/latency", nil)
	req.AddCookie(mintSession(t, svc, "octocat"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}
