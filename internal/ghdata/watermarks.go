package ghdata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Ordering webhook deliveries against each other.
//
// GitHub sends each delivery as it is produced and orders nothing. Two views
// of one resource can arrive reversed -- a retry racing a fresh event, a
// connection reset, a restart draining a queue -- and the older one applied
// second is a correct, repeatable write of superseded state. Idempotence does
// not help: replaying an old payload writes exactly what it says, which is the
// problem.
//
// The fix is a watermark per SUBJECT (the resource a delivery is a view of,
// keyed at the grain that genuinely supersedes -- webhook.OrderOf). A view
// newer than the watermark applies and advances it; a view older than it is
// SUPERSEDED and must not write. Payloads are full snapshots rather than
// deltas, so refusing the older one lands on the identical final state that
// applying them in order would have -- with no delay added to the newer one,
// which matters because GitHub's own delivery timeout is measured in seconds.

// WatermarkRetention bounds how long a subject's watermark is kept. Past it
// there is nothing left for an out-of-order delivery to corrupt: every cache
// it could have moved has expired on its own TTL and the periodic refresh has
// been through since, so remembering the subject costs a row for nothing.
const WatermarkRetention = 7 * 24 * time.Hour

// OrderVerdict is what the watermark said about one delivery.
type OrderVerdict struct {
	// Superseded is true when a strictly newer view of this subject has
	// already been applied. The delivery must not write.
	Superseded bool
	// Previous is the watermark this delivery was measured against (zero when
	// the subject was unknown).
	Previous time.Time
	// PreviousDelivery is the X-GitHub-Delivery that set Previous, so an
	// out-of-order report can name both sides.
	PreviousDelivery string
	// Lateness is how far behind the watermark this view is (zero unless
	// Superseded).
	Lateness time.Duration
}

// ClaimEventOrder decides whether a delivery may write, and advances the
// subject's watermark when it may.
//
// EQUAL times apply. GitHub's payload clocks are second-granular, so two
// genuinely distinct views of one subject can share a timestamp; refusing on
// equality would drop the second one on nothing more than rounding. The cost
// of applying it is the pre-existing last-writer-wins behavior, bounded to
// one second.
//
// The advance is a conditional upsert, so concurrent deliveries for one
// subject are resolved by the payload clock rather than by which transaction
// commits first. The read that reports Previous is best-effort context for the
// out-of-order surfaces; the decision itself never depends on it.
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
		// The stored watermark is equal or newer. Equal is not superseded (see
		// the doc comment): the truncation the watermark is stored at is the
		// same one `at` gets, so compare on that footing.
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
