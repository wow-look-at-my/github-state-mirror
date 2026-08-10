package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/ghclient"
)

// A delivery this mirror never received is the quietest failure it has.
//
// Every cache here is kept honest by webhooks: a push moves a branch tip, the
// delivery applies it, and the answers that depended on the old tip stop being
// served. Miss ONE delivery and none of that happens -- and nothing anywhere
// says so. The mirror keeps serving what it last absorbed, for the full TTL,
// and the consumer reading it cannot tell. The worst shape of it is an answer
// that stops its own repair: pr-minder asks whether a PR is behind its base,
// reads a comparison computed before the base moved, and concludes there is
// nothing to update -- so the PR sits, armed to merge, until someone notices
// by hand. That is not self-correcting, because the stale answer is exactly
// what ends the work that would produce a fresh one.
//
// GitHub does not retry a failed delivery. It does keep a log of them, and it
// will send one again on request. So the gap is recoverable, and the mirror
// has the credential to do it: the replayer reads the App's own failure log
// and asks for every delivery it has not already asked for. The replay
// arrives as an ordinary delivery through the ordinary handler.
//
// A replay is an OLD view, not a fresh one: GitHub re-sends the payload it
// built when the event happened. Applying it after the resource has moved on
// writes state that is wrong now -- a merged PR came back open exactly this
// way. Refusing that is the WRITE's job, not this file's (see ghdata's PR
// closure record): an ordinary late delivery does the same damage, and no
// amount of care here would catch one.
//
// What this does NOT cover, stated plainly: a delivery GitHub records as
// SUCCESSFUL but that this mirror failed to act on (a handler error, a write
// that lost a race). Those are the dispatcher's own dispositions and are
// visible in the delivery log; this recovers the ones that never arrived.

const (
	// ReplayLookback bounds how far back a failure is worth replaying. Past
	// it the caches a delivery would have moved have expired on their own
	// TTLs, so a replay would apply state that nothing is serving stale
	// anymore -- and the periodic fleet refresh has been through since.
	ReplayLookback = 24 * time.Hour

	// ReplayPerCycle caps the requests one cycle may make. A GitHub-side
	// outage, or a restart during a busy minute, can fail deliveries in
	// bulk; the cap keeps the recovery from arriving as a flood of its own.
	// What is skipped is LOGGED, and the next cycle takes the next batch --
	// nothing is silently dropped.
	ReplayPerCycle = 25
)

// DeliveryReplayer asks GitHub to re-send the deliveries it could not hand
// over. Nil-safe: with no App configured there is no credential to read the
// failure log with, and Start returns immediately.
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
