package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// A delivery this mirror never received is the quietest failure it has: the
// mirror keeps serving what it last absorbed, with nothing anywhere saying so.
// see docs/webhooks/delivery-gaps.md

const (
	// ReplayLookback bounds how far back a failure is worth replaying; past it the caches it would move have already expired.
	ReplayLookback = 24 * time.Hour

	// ReplayPerCycle caps requests per cycle so a bulk failure does not arrive as a flood of its own; what is skipped is logged.
	ReplayPerCycle = 25
)

// DeliveryReplayer asks GitHub to resend deliveries it could not hand over; nil-safe when no App is configured.
type DeliveryReplayer struct {
	app      *ghclient.AppAuthenticator
	store    ReplayStore
	interval time.Duration
}

// ReplayStore is the bookkeeping the replayer needs: which deliveries it has
// already asked for, so a failure that stays listed is asked for once.
type ReplayStore interface {
	WebhookReplayRequested(ctx context.Context, deliveryID int64) (bool, error)
	RecordWebhookReplay(ctx context.Context, deliveryID int64, guid, eventType string, deliveredAt, now time.Time) error
	PruneWebhookReplays(ctx context.Context, cutoff time.Time) error
}

func NewDeliveryReplayer(app *ghclient.AppAuthenticator, store ReplayStore, interval time.Duration) *DeliveryReplayer {
	return &DeliveryReplayer{app: app, store: store, interval: interval}
}

// Start runs one recovery cycle immediately, then one per interval, until ctx
// is canceled. The immediate run is the point: a restart is itself a window in
// which deliveries fail, so the first thing a fresh process should do is ask
// what it missed while it was not listening.
func (d *DeliveryReplayer) Start(ctx context.Context) {
	if d == nil || d.app == nil || d.store == nil || d.interval <= 0 {
		return
	}
	if ctx.Err() == nil {
		d.RunOnce(ctx)
	}
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.RunOnce(ctx)
		}
	}
}

// RunOnce reads the failure log and requests what it has not requested
// before. It reports how many replays it asked for.
func (d *DeliveryReplayer) RunOnce(ctx context.Context) int {
	if d == nil || d.app == nil || d.store == nil {
		return 0
	}
	failures, err := d.app.FailedHookDeliveries(ctx)
	if err != nil {
		slog.Error("webhook replay: reading the delivery failure log failed; missed deliveries stay missed this cycle", "error", err)
		return 0
	}
	now := time.Now()
	cutoff := now.Add(-ReplayLookback)
	if err := d.store.PruneWebhookReplays(ctx, cutoff); err != nil {
		slog.Warn("webhook replay: pruning replay bookkeeping failed", "error", err)
	}

	requested, skipped := 0, 0
	for _, f := range failures {
		if !f.DeliveredAt.IsZero() && f.DeliveredAt.Before(cutoff) {
			continue // older than anything still serving stale
		}
		asked, err := d.store.WebhookReplayRequested(ctx, f.ID)
		if err != nil {
			slog.Warn("webhook replay: replay bookkeeping read failed", "delivery", f.ID, "error", err)
			continue
		}
		if asked {
			continue
		}
		if requested >= ReplayPerCycle {
			skipped++
			continue
		}
		// Recorded BEFORE the request: see RecordWebhookReplay for why an
		// unseen outcome must not become a repeat request.
		if err := d.store.RecordWebhookReplay(ctx, f.ID, f.GUID, f.Event, f.DeliveredAt, now); err != nil {
			slog.Warn("webhook replay: recording the request failed; skipping to avoid a repeat", "delivery", f.ID, "error", err)
			continue
		}
		if err := d.app.RedeliverHook(ctx, f.ID); err != nil {
			slog.Error("webhook replay: GitHub refused to re-send a delivery this mirror never got",
				"delivery", f.ID, "guid", f.GUID, "event", f.Event, "error", err)
			continue
		}
		requested++
		slog.Warn("webhook replay: re-sending a delivery this mirror never received",
			"delivery", f.ID, "guid", f.GUID, "event", f.Event, "action", f.Action,
			"status", f.Status, "status_code", f.StatusCode, "delivered_at", f.DeliveredAt)
	}
	if skipped > 0 {
		slog.Error("webhook replay: per-cycle cap reached; missed deliveries remain unreplayed until the next cycle",
			"replayed", requested, "left", skipped, "cap", ReplayPerCycle)
	}
	if requested > 0 {
		slog.Warn("webhook replay: cycle finished", "replayed", requested)
	}
	return requested
}
