package api

import (
	"log/slog"
	"net/http"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
)

// The dashboard's Webhooks tab: the delivery log, plus the check for event
// subscriptions the mirror depends on and has not seen. A missing
// subscription is the quietest failure this service has -- the caches that
// need it simply re-fetch forever -- so it is surfaced here rather than left
// to be inferred from an absence in the table below it.

type webhooksResponse struct {
	Deliveries []ghdata.WebhookDelivery `json:"deliveries"`
	// MissingSubscriptions names the event subscriptions the mirror depends
	// on that have not appeared in the retained delivery log, each with what
	// is degraded without it. A missing subscription is otherwise invisible
	// -- the affected caches just re-fetch forever -- so it is reported
	// rather than left to be discovered.
	MissingSubscriptions []missingSubscription `json:"missing_subscriptions,omitempty"`
}

type missingSubscription struct {
	Event  string `json:"event"`
	Effect string `json:"effect"`
}

// handleWebhooks returns the recent webhook deliveries and their dispositions.
// The delivery log is global (it spans every repo/tenant), so — unlike the
// per-scope cache stats — it is restricted to admins, consistent with the
// admin-only "all scopes" view.
func (d *dashboard) handleWebhooks(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	deliveries, err := d.store.RecentWebhookDeliveries(r.Context(), 100)
	if err != nil {
		slog.Warn("list webhook deliveries failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if deliveries == nil {
		deliveries = []ghdata.WebhookDelivery{}
	}
	// Best-effort: a failed check must not hide the deliveries themselves,
	// but it is logged rather than swallowed.
	var missing []missingSubscription
	if names, err := d.store.MissingWebhookSubscriptions(r.Context()); err != nil {
		slog.Warn("check webhook subscriptions failed", "error", err)
	} else {
		for _, name := range names {
			for _, req := range ghdata.RequiredWebhookEvents {
				if req.Event == name {
					missing = append(missing, missingSubscription{Event: req.Event, Effect: req.Effect})
					break
				}
			}
		}
	}
	writeJSON(w, webhooksResponse{Deliveries: deliveries, MissingSubscriptions: missing})
}
