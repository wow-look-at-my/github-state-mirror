// The dashboard's Webhooks tab: the delivery log, plus the check for event subscriptions the mirror depends on and has not seen.

package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
	"github.com/wow-look-at-my/github-state-mirror/internal/ghdata"
	syncpkg "github.com/wow-look-at-my/github-state-mirror/internal/sync"
	"github.com/wow-look-at-my/go-containers/set"
)

// The dashboard's Webhooks tab: the delivery log, plus the check for event
// subscriptions the mirror depends on and has not seen.

type webhooksResponse struct {
	Deliveries []ghdata.WebhookDelivery `json:"deliveries"`
	// Omitted, never empty, when the App cannot be asked -- see docs/dashboard/dashboard.md.
	MissingSubscriptions []missingSubscription `json:"missing_subscriptions,omitempty"`
	// See docs/webhooks/ordering.md for what this reports and why.
	Ordering *syncpkg.OrderingSnapshot `json:"ordering,omitempty"`
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
	resp, err := d.webhooksSnapshot(r.Context())
	if err != nil {
		slog.Warn("list webhook deliveries failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

// webhookWindow is how many deliveries the Webhooks tab shows.
const webhookWindow = 100

// webhooksSnapshot is the Webhooks tab's whole payload.
func (d *dashboard) webhooksSnapshot(ctx context.Context) (webhooksResponse, error) {
	deliveries, err := d.store.RecentWebhookDeliveries(ctx, webhookWindow)
	if err != nil {
		return webhooksResponse{}, err
	}
	if deliveries == nil {
		deliveries = []ghdata.WebhookDelivery{}
	}
	var ordering *syncpkg.OrderingSnapshot
	if d.ordering != nil {
		snap := d.ordering()
		ordering = &snap
	}
	return webhooksResponse{Deliveries: deliveries, MissingSubscriptions: d.missingSubscriptions(ctx), Ordering: ordering}, nil
}

// missingSubscriptions asks the App which events it is subscribed to and
// reports the required ones it lacks. Returns nil -- the panel disappears --
// whenever the answer cannot be obtained (no App configured, or the call
// failed). This is because the alternative is asserting a configuration
// problem the mirror has no evidence for.
func (d *dashboard) missingSubscriptions(ctx context.Context) []missingSubscription {
	if d.appEvents == nil {
		return nil // no App credential: unknowable, so claim nothing
	}
	subscribed, err := d.subscribed.get(subscribedTTL, func() ([]string, error) { return d.appEvents(ctx) })
	if err != nil {
		// Loud, but never fatal to the tab: the deliveries below still render.
		slog.Warn("read app event subscriptions failed; not reporting subscription state", "error", err)
		return nil
	}
	have := set.Of(subscribed...)
	var missing []missingSubscription
	for _, req := range ghdata.RequiredWebhookEvents {
		if have.Contains(req.Event) || ghclient.AlwaysDeliveredEvents.Contains(req.Event) {
			continue
		}
		missing = append(missing, missingSubscription{Event: req.Event, Effect: req.Effect})
	}
	return missing
}
