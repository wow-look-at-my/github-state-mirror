package ghdata

import (
	"context"
	"time"

	"github.com/wow-look-at-my/github-state-mirror/internal/database/dbgen"
)

// Bookkeeping for delivery-gap recovery. see docs/webhooks/delivery-gaps.md

// WebhookReplayRequested reports whether a replay has already been requested for this delivery id.
func (s *Store) WebhookReplayRequested(ctx context.Context, deliveryID int64) (bool, error) {
	found, err := s.q.WebhookReplayRequested(ctx, deliveryID)
	return found != 0, err
}

// RecordWebhookReplay remembers that a replay was asked for. It is written
// BEFORE the request goes out: a request whose outcome we did not see may
// well have been accepted, and asking again on the next cycle is the failure
// mode worth avoiding. A genuinely lost request is covered by the delivery
// staying in GitHub's failure log for the operator to see.
func (s *Store) RecordWebhookReplay(ctx context.Context, deliveryID int64, guid, eventType string, deliveredAt, now time.Time) error {
	return s.q.RecordWebhookReplay(ctx, dbgen.RecordWebhookReplayParams{
		DeliveryID:  deliveryID,
		Guid:        guid,
		EventType:   eventType,
		DeliveredAt: rfc3339(deliveredAt),
		RequestedAt: rfc3339(now),
	})
}

// PruneWebhookReplays drops rows older than the cutoff -- deliveries the
// replayer will not consider again anyway.
func (s *Store) PruneWebhookReplays(ctx context.Context, cutoff time.Time) error {
	return s.q.PruneWebhookReplays(ctx, rfc3339(cutoff))
}
