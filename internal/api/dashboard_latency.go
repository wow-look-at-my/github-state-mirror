// GitHubDeliveryTimeoutMs is how long GitHub waits for a webhook response before it records the delivery as failed.

package api

import (
	"net/http"

	"github.com/wow-look-at-my/github-state-mirror/internal/latency"
)

// GitHubDeliveryTimeoutMs is how long GitHub waits for a webhook response before it records the delivery as failed.
const GitHubDeliveryTimeoutMs = 10_000

type latencyResponse struct {
	latency.Snapshot
	WebhookTimeoutMs float64 `json:"webhook_timeout_ms"`
}

// handleLatency serves the Latency tab (admin only). The histograms are in-memory and reset on restart.
func (d *dashboard) handleLatency(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.requireAdmin(w, r); !ok {
		return
	}
	writeJSON(w, latencyResponse{Snapshot: d.latency.Snapshot(), WebhookTimeoutMs: GitHubDeliveryTimeoutMs})
}
