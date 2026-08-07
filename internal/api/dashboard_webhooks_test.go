package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	"github.com/wow-look-at-my/github-state-mirror/internal/webhook"
)

// The dashboard's Webhooks tab: the admin-only delivery log, and the report
// of event subscriptions the mirror depends on but has not seen.

func TestDashboard_Webhooks_Admin(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)
	require.NoError(t, store.RecordWebhookDelivery(context.Background(), ghdata.WebhookDelivery{
		DeliveryID:  "abc123",
		EventType:   "pull_request",
		Action:      "opened",
		Repo:        "o/r",
		Disposition: webhook.DispApplied,
		Detail:      "upserted PR #5",
	}))

	req := httptest.NewRequest("GET", "/api/webhooks", nil)
	req.AddCookie(mintSession(t, svc, "PazerOP"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp webhooksResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Deliveries, 1)
	assert.Equal(t, "abc123", resp.Deliveries[0].DeliveryID)
	assert.Equal(t, webhook.DispApplied, resp.Deliveries[0].Disposition)
}

// TestDashboard_Webhooks_ReportsMissingSubscriptions: a GitHub App event the
// mirror depends on but is not subscribed to degrades it SILENTLY -- the
// caches that need it just re-fetch forever, and nothing anywhere says so.
// The delivery log is the evidence, so the dashboard reports the gap, naming
// the event AND what it costs.
func TestDashboard_Webhooks_ReportsMissingSubscriptions(t *testing.T) {
	svc := configuredAuth(t)
	router, store, _ := newTestStack(t, svc)

	get := func() webhooksResponse {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/webhooks", nil)
		req.AddCookie(mintSession(t, svc, "PazerOP"))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var resp webhooksResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		return resp
	}

	// Nothing delivered yet: every required subscription is reported, each
	// with the consequence of its absence.
	resp := get()
	require.Len(t, resp.MissingSubscriptions, len(ghdata.RequiredWebhookEvents))
	byEvent := map[string]string{}
	for _, m := range resp.MissingSubscriptions {
		byEvent[m.Event] = m.Effect
		assert.NotEmpty(t, m.Effect, "%s must say what breaks without it", m.Event)
	}
	assert.Contains(t, byEvent, "workflow_run",
		"workflow_run is load-bearing for the runs listing and must be reported when absent")

	// One delivery of that type is the evidence it IS subscribed, so it stops
	// being reported -- and the others still are.
	require.NoError(t, store.RecordWebhookDelivery(context.Background(), ghdata.WebhookDelivery{
		DeliveryID: "wr-1", EventType: "workflow_run", Action: "completed",
		Repo: "o/r", Disposition: webhook.DispApplied,
	}))
	resp = get()
	for _, m := range resp.MissingSubscriptions {
		assert.NotEqual(t, "workflow_run", m.Event, "a delivered event must not be reported missing")
	}
	assert.Len(t, resp.MissingSubscriptions, len(ghdata.RequiredWebhookEvents)-1,
		"only the delivered event drops off the list")
}

func TestDashboard_Webhooks_NonAdminForbidden(t *testing.T) {
	svc := configuredAuth(t)
	router, store, db := newTestStack(t, svc)
	seedPrincipal(t, store, db, "user:100", "octocat")

	req := httptest.NewRequest("GET", "/api/webhooks", nil)
	req.AddCookie(mintSession(t, svc, "octocat")) // not an admin
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDashboard_Webhooks_Unauthenticated(t *testing.T) {
	svc := configuredAuth(t)
	router, _, _ := newTestStack(t, svc)

	req := httptest.NewRequest("GET", "/api/webhooks", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
