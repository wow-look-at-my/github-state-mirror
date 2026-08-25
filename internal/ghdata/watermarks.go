package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Ordering webhook deliveries against each other: a watermark per SUBJECT, applied and advanced by newer views, refusing older ones.
// see docs/webhooks/ordering.md

// WatermarkRetention bounds how long a subject's watermark is kept; past it every cache it could corrupt has already expired or refreshed.
const WatermarkRetention = 7 * 24 * time.Hour

// OrderVerdict is what the watermark said about one delivery.
type OrderVerdict struct {
	// Superseded is true when a strictly newer view of this subject already applied; the delivery must not write.
	Superseded bool
	// Previous is the watermark this delivery was measured against (zero when the subject was unknown).
	Previous time.Time
	// PreviousDelivery is the X-GitHub-Delivery that set Previous, so an out-of-order report can name both sides.
	PreviousDelivery string
	// Lateness is how far behind the watermark this view is (zero unless Superseded).
	Lateness time.Duration
}

// ClaimEventOrder decides whether a delivery may write, and advances the subject's watermark when it may. EQUAL times apply.
// see docs/webhooks/ordering.md
func (s *Store) ClaimEventOrder(ctx context.Context, subject string, at time.Time, deliveryID, eventType string, now time.Time) (OrderVerdict, error) {
	if subject == "" || at.IsZero() {
		return OrderVerdict{}, nil // unorderable: applies, as it always did
	}
	previous, previousDelivery := time.Time{}, ""
	if row, err := s.q.GetWebhookWatermark(ctx, subject); err == nil {
		if prev, perr := time.Parse(time.RFC3339, row.EventTime); perr == nil {
			previous, previousDelivery = prev, row.DeliveryID
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return OrderVerdict{}, err
	}

	_, err := s.q.ClaimWebhookWatermark(ctx, dbgen.ClaimWebhookWatermarkParams{
		Subject: subject, EventTime: rfc3339(at.UTC()), DeliveryID: deliveryID,
		EventType: eventType, UpdatedAt: rfc3339(now),
	})
	switch {
	case err == nil:
		// Won the claim: this is the newest view of the subject.
		_ = s.q.PruneWebhookWatermarks(ctx, rfc3339(now.Add(-WatermarkRetention)))
		return OrderVerdict{Previous: previous, PreviousDelivery: previousDelivery}, nil
	case errors.Is(err, sql.ErrNoRows):
		// The stored watermark is equal or newer; equal is not superseded since both truncate to the same footing.
		if !previous.IsZero() && !at.UTC().Truncate(time.Second).Before(previous) {
			return OrderVerdict{Previous: previous, PreviousDelivery: previousDelivery}, nil
		}
		return OrderVerdict{
			Superseded: true, Previous: previous, PreviousDelivery: previousDelivery,
			Lateness: previous.Sub(at.UTC()),
		}, nil
	default:
		return OrderVerdict{}, err
	}
}
