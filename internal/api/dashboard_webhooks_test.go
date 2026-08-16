package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
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

// The missing-subscription report must come from the APP, never from the
// delivery log. Inferring it from traffic reads "this event has not arrived
// lately" as "this event is not configured" -- and the retained log is bounded
// by row count, so on a CI-heavy fleet it spans minutes and every
// low-frequency required event (repository, label) is reported missing
// forever while correctly subscribed. That guard cried wolf on every page
// load, which is strictly worse than no guard: it trains the operator to skim
// past the one time a subscription really is gone.
func TestMissingSubscriptions_ComeFromTheAppNotTheTrafficLog(t *testing.T) {
	// GitHub's answer: subscribed to everything required EXCEPT "label".
	// "installation" is deliberately absent from the list too -- GitHub never
	// lists it, because every App receives it unconditionally.
	subscribed := []string{}
	for _, req := range ghdata.RequiredWebhookEvents {
		if req.Event == "label" || ghclient.AlwaysDeliveredEvents.Contains(req.Event) {
			continue
		}
		subscribed = append(subscribed, req.Event)
	}
	d := &dashboard{appEvents: func(context.Context) ([]string, error) { return subscribed, nil }}

	missing := d.missingSubscriptions(httptest.NewRequest("GET", "/api/webhooks", nil))

	require.Len(t, missing, 1, "only the genuinely unsubscribed event may be reported")
	assert.Equal(t, "label", missing[0].Event)
	assert.NotEmpty(t, missing[0].Effect, "a reported event must say what breaks without it")
	for _, m := range missing {
		assert.NotEqual(t, "installation", m.Event,
			"installation is delivered to every App unconditionally and can never be unsubscribed")
	}
}

// A fully-subscribed App reports NOTHING, however quiet the delivery log is.
// This is the exact false alarm the old traffic-inference produced.
func TestMissingSubscriptions_SilentButSubscribedReportsNothing(t *testing.T) {
	all := []string{}
	for _, req := range ghdata.RequiredWebhookEvents {
		all = append(all, req.Event)
	}
	d := &dashboard{appEvents: func(context.Context) ([]string, error) { return all, nil }}
	assert.Empty(t, d.missingSubscriptions(httptest.NewRequest("GET", "/api/webhooks", nil)),
		"an idle but subscribed fleet must produce no subscription warnings")
}

// Unknowable is not the same as missing: with no App configured, or when the
// call fails, the panel says nothing rather than asserting a configuration
// problem the mirror has no evidence for.
func TestMissingSubscriptions_UnknownIsNotMissing(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/webhooks", nil)

	noApp := &dashboard{}
	assert.Nil(t, noApp.missingSubscriptions(r), "no App credential: claim nothing")

	failing := &dashboard{appEvents: func(context.Context) ([]string, error) {
		return nil, errors.New("github unreachable")
	}}
	assert.Nil(t, failing.missingSubscriptions(r), "a failed lookup must not read as 'all missing'")
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
